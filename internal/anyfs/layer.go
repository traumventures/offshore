package anyfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

// LayerVersion is the layer format version.
const LayerVersion = 1

// Layer is a committed delta of a tree: the changed nodes and whiteouts
// on top of its parent (Base layers have none and hold a full tree).
type Layer struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Parent  string `json:"parent,omitempty"`
	At      int64  `json:"at"` // unix seconds of the commit
	// Created is the unix time of the oldest data in the tree (the age
	// that counts against the build slot TTL; kept by compaction).
	Created int64 `json:"created"`
	// Base marks a full tree (no parent): the result of compaction.
	Base bool `json:"base,omitempty"`
	// Depth is the number of layers below.
	Depth int `json:"depth,omitempty"`
	// Bytes is the size of the whole tree this layer completes, Delta
	// the bytes of its changed files.
	Bytes int64  `json:"bytes"`
	Delta int64  `json:"delta,omitempty"`
	Meta  []byte `json:"meta,omitempty"` // opaque data of the owner
	Nodes []Node `json:"nodes"`
}

// NewLayerID returns a fresh, time ordered layer id.
func NewLayerID(now time.Time) string {
	var r [4]byte
	rand.Read(r[:])
	return fmt.Sprintf("%d-%s", now.Unix(), hex.EncodeToString(r[:]))
}

// ErrNoLayer means a layer or head is missing (or expired).
var ErrNoLayer = errors.New("anyfs: no such layer")

// LayerObject is the object key of a layer.
func LayerObject(id string) string  { return "layers/" + id + ".json.gz" }
func headObject(name string) string { return "heads/" + name }

// SaveLayer writes a layer object.
func SaveLayer(ctx context.Context, st blobcache.Store, l *Layer) error {
	l.Version = LayerVersion
	var buf bytes.Buffer
	if err := Encode(&buf, l); err != nil {
		return err
	}
	return st.Put(ctx, LayerObject(l.ID), bytes.NewReader(buf.Bytes()), int64(buf.Len()))
}

// LoadLayer reads a layer object.
func LoadLayer(ctx context.Context, st blobcache.Store, id string) (*Layer, error) {
	rc, _, err := st.Get(ctx, LayerObject(id))
	if err != nil {
		if errors.Is(err, blobcache.ErrNotFound) {
			return nil, fmt.Errorf("layer %s: %w", id, ErrNoLayer)
		}
		return nil, err
	}
	defer rc.Close()
	var l Layer
	if err := Decode(rc, &l); err != nil {
		return nil, fmt.Errorf("layer %s: %w", id, err)
	}
	if l.Version != LayerVersion {
		return nil, fmt.Errorf("layer %s: unsupported version %d", id, l.Version)
	}
	return &l, nil
}

// DeleteLayer removes a layer object (pages expire with the store TTL).
func DeleteLayer(ctx context.Context, st blobcache.Store, id string) error {
	return st.Delete(ctx, LayerObject(id))
}

// SetHead points a named head (e.g. a repository's slot lineage) at a
// layer.
func SetHead(ctx context.Context, st blobcache.Store, name, id string) error {
	return st.Put(ctx, headObject(name), strings.NewReader(id), int64(len(id)))
}

// Head returns the layer a head points at.
func Head(ctx context.Context, st blobcache.Store, name string) (string, error) {
	rc, _, err := st.Get(ctx, headObject(name))
	if err != nil {
		if errors.Is(err, blobcache.ErrNotFound) {
			return "", fmt.Errorf("head %s: %w", name, ErrNoLayer)
		}
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 256))
	return strings.TrimSpace(string(b)), err
}

// Chain is a stack of layers, bottom (a base) first.
type Chain []*Layer

// LoadChain loads a layer and its ancestors down to a base.
func LoadChain(ctx context.Context, st blobcache.Store, id string) (Chain, error) {
	var c Chain
	for id != "" {
		l, err := LoadLayer(ctx, st, id)
		if err != nil {
			return nil, err
		}
		c = append(Chain{l}, c...)
		if l.Base {
			break
		}
		id = l.Parent
		if len(c) > 10000 {
			return nil, errors.New("anyfs: layer chain too long")
		}
	}
	return c, nil
}

// Top returns the top layer (nil for an empty chain).
func (c Chain) Top() *Layer {
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

// IDs returns the layer ids bottom first.
func (c Chain) IDs() []string {
	out := make([]string, len(c))
	for i, l := range c {
		out[i] = l.ID
	}
	return out
}

// Tree folds the chain into its tree.
func (c Chain) Tree() *Tree {
	ls := make([][]Node, len(c))
	for i, l := range c {
		ls[i] = l.Nodes
	}
	return Merge(ls...)
}

// Commit makes a layer of the change from base (the chain's tree) to top.
func Commit(c Chain, base, top *Tree, now time.Time, meta []byte) *Layer {
	nodes := Diff(base, top)
	l := &Layer{ID: NewLayerID(now), At: now.Unix(), Created: now.Unix(), Bytes: top.Bytes(), Meta: meta, Nodes: nodes}
	if p := c.Top(); p != nil {
		l.Parent, l.Depth, l.Created = p.ID, p.Depth+1, c.Oldest().Unix()
	} else {
		l.Base = true
	}
	for _, n := range nodes {
		if n.Kind == File {
			l.Delta += n.Size
		}
	}
	return l
}

// Compact folds a chain into one base layer with the top's meta.
func Compact(c Chain, now time.Time) *Layer {
	t := c.Tree()
	l := &Layer{ID: NewLayerID(now), At: now.Unix(), Created: c.Oldest().Unix(), Base: true, Bytes: t.Bytes(), Delta: t.Bytes(),
		Nodes: append([]Node(nil), t.Nodes()...)}
	if top := c.Top(); top != nil {
		l.Meta = top.Meta
	}
	return l
}

// Oldest returns the commit time of the bottom layer: the age of the
// oldest data in the chain.
func (c Chain) Oldest() time.Time {
	if len(c) == 0 {
		return time.Time{}
	}
	if c[0].Created > 0 {
		return time.Unix(c[0].Created, 0)
	}
	return time.Unix(c[0].At, 0)
}
