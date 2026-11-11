package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// blobTiers resolves a digest to a local file: local CAS (cache) first, then
// the backing store. A store with latency/bandwidth simulates an object store
// (S3 restore); with cache == "" the store dir itself is local and its blobs
// are opened in place.
type blobTiers struct {
	store     string
	cache     string
	latency   time.Duration
	bandwidth float64 // bytes/s, 0 = unlimited

	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]*fetchCall
	pipeFree time.Time

	fetches      atomic.Int64
	fetchedBytes atomic.Int64
	fetchNs      atomic.Int64 // summed wall time callers waited for fetches
	cacheHits    atomic.Int64
}

type fetchCall struct {
	done chan struct{}
	err  error
}

func newBlobTiers(store, cache string, latency time.Duration, bw float64, parallel int) *blobTiers {
	return &blobTiers{store: store, cache: cache, latency: latency, bandwidth: bw,
		sem: make(chan struct{}, parallel), inflight: map[string]*fetchCall{}}
}

// local reports the local path of a blob if no fetch is needed.
func (b *blobTiers) local(digest string) (string, bool) {
	if b.cache == "" {
		return filepath.Join(b.store, digest), true
	}
	p := filepath.Join(b.cache, digest)
	if _, err := os.Stat(p); err == nil {
		return p, true
	}
	return "", false
}

func (b *blobTiers) path(digest string, size int64) (string, error) {
	if b.cache == "" {
		return filepath.Join(b.store, digest), nil
	}
	local := filepath.Join(b.cache, digest)
	if _, err := os.Stat(local); err == nil {
		b.cacheHits.Add(1)
		return local, nil
	}
	start := time.Now()
	defer func() { b.fetchNs.Add(int64(time.Since(start))) }()
	b.mu.Lock()
	if c, ok := b.inflight[digest]; ok {
		b.mu.Unlock()
		<-c.done
		return local, c.err
	}
	c := &fetchCall{done: make(chan struct{})}
	b.inflight[digest] = c
	b.mu.Unlock()

	c.err = b.fetch(digest, size, local)
	close(c.done)
	b.mu.Lock()
	delete(b.inflight, digest)
	b.mu.Unlock()
	return local, c.err
}

func (b *blobTiers) fetch(digest string, size int64, local string) error {
	b.sem <- struct{}{}
	defer func() { <-b.sem }()
	time.Sleep(b.latency) // per-object request latency
	if b.bandwidth > 0 {
		// One shared pipe: transfers are serialised at the configured rate.
		b.mu.Lock()
		now := time.Now()
		if b.pipeFree.Before(now) {
			b.pipeFree = now
		}
		b.pipeFree = b.pipeFree.Add(time.Duration(float64(size) / b.bandwidth * float64(time.Second)))
		until := b.pipeFree
		b.mu.Unlock()
		time.Sleep(time.Until(until))
	}
	if err := copyFile(filepath.Join(b.store, digest), local); err != nil {
		return err
	}
	b.fetches.Add(1)
	b.fetchedBytes.Add(size)
	return nil
}
