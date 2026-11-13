package anyfs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/zeebo/blake3"

	"github.com/traumventures/offshore/internal/medium"
)

// Key identifies a page by content: the hex BLAKE3 hash of its bytes.
type Key string

// KeyOf returns the key of a page's content.
func KeyOf(b []byte) Key {
	s := blake3.Sum256(b)
	return Key(hex.EncodeToString(s[:]))
}

// Valid reports whether k looks like a page key.
func (k Key) Valid() bool {
	if len(k) != 64 {
		return false
	}
	_, err := hex.DecodeString(string(k))
	return err == nil
}

// Short is the key prefix used in logs.
func (k Key) Short() string {
	if len(k) < 12 {
		return string(k)
	}
	return string(k[:12])
}

// PageRef is a byte range of a page holding (part of) a file.
type PageRef struct {
	Key Key   `json:"h"`
	Off int64 `json:"o,omitempty"`
	Len int64 `json:"l"`
}

// State is a resident page's state relative to the tier below.
type State uint8

const (
	// Clean pages are held by the lower tier too; reclaim is free.
	Clean State = iota
	// Dirty pages must be written back before they may be reclaimed.
	Dirty
)

func (s State) String() string {
	if s == Dirty {
		return "dirty"
	}
	return "clean"
}

// PageSize is the target size of a page.
const PageSize = medium.PageSize

// SmallFile is the largest file packed together with others.
const SmallFile = 1 << 20

// PageSink receives the pages a Packer produces (dirty: not yet in any
// lower tier).
type PageSink func(k Key, b []byte) error

// Range is where a large file's page lies in a local file: a tier on the
// same file system can store the page as a clone of the range (reflink,
// metadata only) instead of writing its bytes a second time.
type Range struct {
	File *os.File
	Off  int64
	// Ctime is the file's ctime (ns) before its content was read: a
	// clone is only kept if the file still has it (0: never cloned).
	Ctime int64
}

// RangeSink receives pages with their range (File nil: none).
type RangeSink func(k Key, b []byte, r Range) error

// RangePutter is implemented by tiers that can store a page as a clone of
// a file range (falling back to writing b).
type RangePutter interface {
	PutRange(ctx context.Context, k Key, b []byte, r Range, s State) error
}

// PutPage stores a page in t, as a clone of r where t supports it.
func PutPage(ctx context.Context, t Tier, k Key, b []byte, r Range, s State) error {
	if rp, ok := t.(RangePutter); ok && r.File != nil {
		return rp.PutRange(ctx, k, b, r, s)
	}
	return t.Put(ctx, k, b, s)
}

// Packer turns file contents into pages: small files are packed into
// pages of about PageSize with content defined boundaries (an inserted
// or removed file only changes the page around it), large files are
// split into PageSize pieces.
type Packer struct {
	sink    PageSink
	ranges  RangeSink // pages of large files with their range (nil: sink)
	serial  bool      // hash on the calling goroutine only (Serial)
	pack    bytes.Buffer
	pending []*Node
	pages   int64
	bytes   int64
}

// NewPacker returns a packer emitting pages to sink.
func NewPacker(sink PageSink) *Packer { return &Packer{sink: sink} }

// Serial makes the packer hash large files on the calling goroutine
// only: a caller running at a low scheduling priority (background
// hashing) keeps all of its work there.
func (p *Packer) Serial() *Packer {
	p.serial = true
	return p
}

// Pages returns the number of pages emitted and their bytes.
func (p *Packer) Pages() (int64, int64) { return p.pages, p.bytes }

// WithRanges makes the packer emit the pages of large files read by
// AddLargeFile to rs, with their range in the file.
func (p *Packer) WithRanges(rs RangeSink) *Packer {
	p.ranges = rs
	return p
}

func (p *Packer) emit(b []byte) (Key, error) {
	return p.emitAt(b, Range{})
}

func (p *Packer) emitAt(b []byte, r Range) (Key, error) {
	k := KeyOf(b)
	p.pages++
	p.bytes += int64(len(b))
	if p.ranges != nil && r.File != nil {
		return k, p.ranges(k, b, r)
	}
	return k, p.sink(k, b)
}

// AddSmall packs a small file's content and sets n.Pages once its page is
// complete (Flush completes the last one). n.Size and n.Digest are set
// from b.
func (p *Packer) AddSmall(n *Node, b []byte) error {
	n.Size = int64(len(b))
	n.Digest = string(KeyOf(b))
	return p.addSmall(n, b)
}

// addSmall is AddSmall with n.Size and n.Digest already set.
func (p *Packer) addSmall(n *Node, b []byte) error {
	n.Pages = nil
	if len(b) == 0 {
		return nil
	}
	p.pack.Write(b)
	p.pending = append(p.pending, n)
	if p.pack.Len() >= PageSize || (p.pack.Len() >= SmallFile && n.Digest[0] == '0') { // first digest byte < 16
		return p.Flush()
	}
	return nil
}

// AddAlone gives a small file's content a page of its own (key = content
// digest: the same file content is the same page wherever it is
// scanned) and sets n.Pages, n.Size and n.Digest.
func (p *Packer) AddAlone(n *Node, b []byte) error {
	n.Size = int64(len(b))
	n.Digest = string(KeyOf(b))
	return p.addAlone(n, b)
}

// addAlone is AddAlone with n.Size and n.Digest already set.
func (p *Packer) addAlone(n *Node, b []byte) error {
	n.Pages = nil
	if len(b) == 0 {
		return nil
	}
	k, err := p.emit(append([]byte(nil), b...))
	if err != nil {
		return err
	}
	n.Pages = []PageRef{{Key: k, Len: n.Size}}
	return nil
}

// AddLarge splits the content read from r into pages and sets n.Pages,
// n.Size and n.Digest.
func (p *Packer) AddLarge(n *Node, r io.Reader) error {
	return p.AddLargeFile(n, nil, 0, r)
}

// AddLargeFile is AddLarge for content read from r at the start of f
// (r may limit the rate of reading f), whose ctime before reading was
// ctime: with WithRanges, the pages carry their range in f.
//
// Every byte is hashed twice (the page key and the file digest): the two
// run on separate cores, and the next page is read while the previous
// one is hashed, so one large file pages at several times one BLAKE3
// core's rate instead of half of it.
func (p *Packer) AddLargeFile(n *Node, f *os.File, ctime int64, r io.Reader) error {
	if p.serial {
		return p.addLargeSerial(n, f, ctime, r)
	}
	type page struct {
		b   []byte
		off int64
		key chan Key
	}
	h := blake3.New()
	pages := make(chan page, 2)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		defer close(pages)
		var off int64
		for {
			b := make([]byte, PageSize)
			k, err := io.ReadFull(r, b)
			if k > 0 {
				pg := page{b: b[:k], off: off, key: make(chan Key, 1)}
				go func() { pg.key <- KeyOf(pg.b) }()
				select {
				case pages <- pg:
				case <-stop:
					readErr <- nil
					return
				}
				off += int64(k)
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					err = nil
				}
				readErr <- err
				return
			}
		}
	}()
	var refs []PageRef
	var total int64
	var perr error
	for pg := range pages {
		if perr != nil {
			continue
		}
		h.Write(pg.b)
		key := <-pg.key
		p.pages++
		p.bytes += int64(len(pg.b))
		if p.ranges != nil && f != nil {
			perr = p.ranges(key, pg.b, Range{File: f, Off: pg.off, Ctime: ctime})
		} else {
			perr = p.sink(key, pg.b)
		}
		if perr != nil {
			close(stop)
			continue
		}
		refs = append(refs, PageRef{Key: key, Len: int64(len(pg.b))})
		total += int64(len(pg.b))
	}
	if err := <-readErr; err != nil {
		return err
	}
	if perr != nil {
		return perr
	}
	n.Pages, n.Size, n.Digest = refs, total, hex.EncodeToString(h.Sum(nil))
	return nil
}

// addLargeSerial is AddLargeFile on the calling goroutine.
func (p *Packer) addLargeSerial(n *Node, f *os.File, ctime int64, r io.Reader) error {
	h := blake3.New()
	buf := make([]byte, PageSize)
	var refs []PageRef
	var total int64
	for {
		k, err := io.ReadFull(r, buf)
		if k > 0 {
			b := append([]byte(nil), buf[:k]...)
			h.Write(b)
			key, perr := p.emitAt(b, Range{File: f, Off: total, Ctime: ctime})
			if perr != nil {
				return perr
			}
			refs = append(refs, PageRef{Key: key, Len: int64(k)})
			total += int64(k)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return err
		}
	}
	n.Pages, n.Size, n.Digest = refs, total, hex.EncodeToString(h.Sum(nil))
	return nil
}

// Flush completes the current pack page.
func (p *Packer) Flush() error {
	if p.pack.Len() == 0 {
		return nil
	}
	b := append([]byte(nil), p.pack.Bytes()...)
	k, err := p.emit(b)
	if err != nil {
		return err
	}
	var off int64
	for _, n := range p.pending {
		n.Pages = []PageRef{{Key: k, Off: off, Len: n.Size}}
		off += n.Size
	}
	p.pack.Reset()
	p.pending = p.pending[:0]
	return nil
}
