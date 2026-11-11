package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/traumventures/offshore/internal/anyfs"
	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/events"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/scheduler"
	"github.com/traumventures/offshore/internal/store"
)

// Build slot heads and layers (internal/anyfs) in the control plane.
//
// Workers commit build slots as layer chains and flush them to the
// stores; they report the flushed heads (proto.Status.Heads, slot save
// results). The orchestrator keeps per repository the newest heads
// (restore and move targets, scheduler cache affinity) and every layer
// it saw in their chains. Garbage collection (mark and sweep) deletes
// layer objects no kept head reaches and those older than the slot TTL;
// pages expire with the store TTL.

// headsKeep bounds the heads remembered per repository.
const headsKeep = 4

// layerTTL is the age after which layers and heads are dropped (the
// build slot TTL and the store's object TTL).
const layerTTL = blobcache.MaxTTL

// gcInterval is how often layer garbage collection runs.
const gcInterval = 10 * time.Minute

// repoLayers are a repository's heads and known layers.
type repoLayers struct {
	Heads []scheduler.Snapshot `json:"heads"` // newest first
	// Chains are the layer chains of the heads (head layer -> ids, bottom
	// first); Created the age of their oldest data.
	Chains  map[string][]string  `json:"chains,omitempty"`
	Created map[string]time.Time `json:"created,omitempty"`
	// Layers are all layers seen in chains, with their store.
	Layers map[string]layerRec `json:"layers,omitempty"`
}

type layerRec struct {
	Provider string    `json:"provider"`
	Store    string    `json:"store"`
	At       time.Time `json:"at"`
}

func (o *Orchestrator) headsPath() string { return filepath.Join(o.stateDir, "layers.json") }

func (o *Orchestrator) loadHeads() {
	b, err := os.ReadFile(o.headsPath())
	if err != nil {
		return
	}
	var m map[string]*repoLayers
	if json.Unmarshal(b, &m) == nil && m != nil {
		o.heads = m
	}
}

func (o *Orchestrator) saveHeadsLocked() {
	b, err := json.Marshal(o.heads)
	if err != nil {
		return
	}
	tmp := o.headsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, o.headsPath())
	}
}

// layerTime is the commit time encoded in a layer id ("<unix>-<rand>").
func layerTime(id string, def time.Time) time.Time {
	var sec int64
	for _, c := range id {
		if c < '0' || c > '9' {
			break
		}
		sec = sec*10 + int64(c-'0')
	}
	if sec == 0 {
		return def
	}
	return time.Unix(sec, 0)
}

// addHeadLocked remembers a flushed head with its chain. It reports
// whether the head is new.
func (o *Orchestrator) addHeadLocked(s scheduler.Snapshot, chain []string, created time.Time) bool {
	if s.Repo == "" || s.At.IsZero() {
		return false
	}
	rl := o.heads[s.Repo]
	if rl == nil {
		rl = &repoLayers{}
		o.heads[s.Repo] = rl
	}
	for _, x := range rl.Heads {
		if x.Store == s.Store && x.At.Equal(s.At) && x.Layer == s.Layer {
			return false
		}
	}
	if rl.Chains == nil {
		rl.Chains, rl.Created, rl.Layers = map[string][]string{}, map[string]time.Time{}, map[string]layerRec{}
	}
	if s.Layer != "" {
		if len(chain) == 0 {
			chain = []string{s.Layer}
		}
		rl.Chains[s.Layer] = chain
		if created.IsZero() {
			created = s.At
		}
		rl.Created[s.Layer] = created
		for _, id := range chain {
			if _, ok := rl.Layers[id]; !ok {
				rl.Layers[id] = layerRec{Provider: s.Provider, Store: s.Store, At: layerTime(id, s.At)}
			}
		}
	}
	rl.Heads = append(rl.Heads, s)
	sort.Slice(rl.Heads, func(i, j int) bool { return rl.Heads[i].At.After(rl.Heads[j].At) })
	for _, x := range rl.Heads[min(headsKeep, len(rl.Heads)):] {
		delete(rl.Chains, x.Layer)
		delete(rl.Created, x.Layer)
	}
	rl.Heads = rl.Heads[:min(headsKeep, len(rl.Heads))]
	o.saveHeadsLocked()
	return true
}

// SnapshotFromHead converts a worker's head description.
func SnapshotFromHead(h proto.LayerHead) scheduler.Snapshot {
	return scheduler.Snapshot{Repo: h.Repo, Provider: h.Provider, Store: h.Store, Bytes: h.Bytes, At: time.Unix(h.At, 0), Layer: h.Layer}
}

// statusHeadsLocked takes the heads a worker reports and logs a writeback
// event for each new one.
func (o *Orchestrator) statusHeadsLocked(id string, m *Machine) {
	if m.Status == nil {
		return
	}
	for _, h := range m.Status.Heads {
		if h.Provider == "" {
			h.Provider = m.Provider
		}
		if !o.addHeadLocked(SnapshotFromHead(h), h.Chain, time.Unix(h.Created, 0)) {
			continue
		}
		ev := o.machineEventLocked(events.Writeback, id)
		ev.Repo, ev.Store, ev.Pages, ev.Bytes, ev.Seconds = h.Repo, h.Store, h.Pages, h.PageBytes, h.Seconds
		o.obs.log.Append(ev)
	}
}

// layerVictim is a layer object to delete.
type layerVictim struct {
	repo, id string
	rec      layerRec
}

// gcLayersLocked drops expired heads and returns the layers to delete:
// those no kept head reaches and those older than the TTL.
func (o *Orchestrator) gcLayersLocked(now time.Time) []layerVictim {
	var out []layerVictim
	changed := false
	for repo, rl := range o.heads {
		keep := rl.Heads[:0]
		reach := map[string]bool{}
		for _, h := range rl.Heads {
			created := rl.Created[h.Layer]
			if created.IsZero() {
				created = h.At
			}
			if now.Sub(created) >= layerTTL {
				delete(rl.Chains, h.Layer)
				delete(rl.Created, h.Layer)
				changed = true
				continue
			}
			keep = append(keep, h)
			for _, id := range rl.Chains[h.Layer] {
				reach[id] = true
			}
		}
		rl.Heads = keep
		for id, rec := range rl.Layers {
			if reach[id] && now.Sub(rec.At) < layerTTL {
				continue
			}
			out = append(out, layerVictim{repo, id, rec})
			delete(rl.Layers, id)
			changed = true
		}
		if len(rl.Heads) == 0 && len(rl.Layers) == 0 {
			delete(o.heads, repo)
			changed = true
		}
	}
	if changed {
		o.saveHeadsLocked()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// GCLayers deletes unreachable and expired layer objects from the stores
// (at most every gcInterval unless forced).
func (o *Orchestrator) GCLayers(ctx context.Context, force bool) int {
	o.mu.Lock()
	if !force && o.now().Sub(o.layersGC) < gcInterval {
		o.mu.Unlock()
		return 0
	}
	o.layersGC = o.now()
	// Layer times are the workers' wall clock (not the orchestrator's
	// test clock).
	victims := o.gcLayersLocked(time.Now())
	stores := o.Stores
	o.mu.Unlock()
	if stores == nil {
		return 0
	}
	n := 0
	opened := map[string]blobcache.Store{}
	for _, v := range victims {
		key := v.rec.Provider + "\x00" + v.repo
		st, ok := opened[key]
		if !ok {
			st = o.repoStore(ctx, stores, v.rec.Provider, v.repo)
			opened[key] = st
		}
		if st == nil {
			continue
		}
		if err := anyfs.DeleteLayer(ctx, st, v.id); err != nil {
			o.log.Warn("delete layer", "repo", v.repo, "layer", v.id, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		o.log.Info("deleted unreachable or expired layers", "count", n)
	}
	return n
}

// repoStore opens a repository's prefix in a provider's store.
func (o *Orchestrator) repoStore(ctx context.Context, stores *StoreSet, prov, repo string) blobcache.Store {
	g, err := stores.Granter(ctx, prov)
	if err != nil || g == nil {
		return nil
	}
	gr, err := g.Grant(ctx, []string{store.RepoScope(repo)}, time.Hour)
	if err != nil {
		o.log.Warn("store grant for layer gc", "provider", prov, "err", err)
		return nil
	}
	st, err := store.Open(gr, store.RepoScope(repo), blobcache.MaxTTL)
	if err != nil {
		return nil
	}
	return st
}

// OutputsStore is what the laptop needs to read a repository's latest
// committed build slot from the object store when the machine of the
// latest build is gone: the newest head and a grant for the
// repository's prefix of its store.
type OutputsStore struct {
	Head  proto.LayerHead `json:"head"`
	Grant store.Grant     `json:"grant"`
}

// ErrNoHead means no committed head of the repository is known.
var ErrNoHead = errors.New("no committed build slot of the repository in a store")

// OutputsStore returns the newest committed head of repo with a grant
// for its store. The grant covers only the repository's prefix, which
// the caller (holding the pool's API key) can already reach through any
// session it leases; it expires with GrantTTL.
func (o *Orchestrator) OutputsStore(ctx context.Context, repo string) (OutputsStore, error) {
	o.mu.Lock()
	var head scheduler.Snapshot
	var chain []string
	if rl := o.heads[repo]; rl != nil {
		for _, h := range rl.Heads {
			if h.Layer != "" {
				head, chain = h, rl.Chains[h.Layer]
				break
			}
		}
	}
	o.mu.Unlock()
	if head.Layer == "" {
		return OutputsStore{}, ErrNoHead
	}
	gr := o.storeGrant(ctx, head.Provider, repo, "")
	if gr == nil {
		return OutputsStore{}, fmt.Errorf("no store grant for provider %s", head.Provider)
	}
	return OutputsStore{Head: proto.LayerHead{Repo: repo, Store: head.Store, Provider: head.Provider, Layer: head.Layer,
		Chain: chain, Bytes: head.Bytes, At: head.At.Unix()}, Grant: *gr}, nil
}
