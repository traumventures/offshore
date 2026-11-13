package store

import (
	"context"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/blobcache"
)

// Stores opened from a grant never serve objects older than the grant's
// retention, whatever TTL the caller asks for.
func TestGrantRetention(t *testing.T) {
	g, err := NewGranter(context.Background(), Config{Name: "local", Dir: t.TempDir(), Prefix: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	gr, _ := g.Grant(context.Background(), []string{RepoScope("r")}, time.Hour)
	if gr.Retention() != blobcache.DefaultTTL || gr.RefreshAge() != 12*time.Hour {
		t.Fatalf("default retention %v refresh %v", gr.Retention(), gr.RefreshAge())
	}
	for _, c := range []struct {
		retention, ask, want time.Duration
	}{
		{0, blobcache.MaxTTL, 24 * time.Hour},
		{72 * time.Hour, blobcache.MaxTTL, 72 * time.Hour},
		{72 * time.Hour, 0, 72 * time.Hour},
		{72 * time.Hour, time.Hour, time.Hour},
		{30 * 24 * time.Hour, 0, blobcache.MaxTTL},
	} {
		gr.RetentionSeconds = int64(c.retention / time.Second)
		st, err := Open(gr, RepoScope("r"), c.ask)
		if err != nil {
			t.Fatal(err)
		}
		if got := blobcache.TTLOf(st); got != c.want {
			t.Errorf("retention %v ask %v: ttl %v, want %v", c.retention, c.ask, got, c.want)
		}
	}
}
