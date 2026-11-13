package anyfs

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestScanThroughput measures commit hashing of a real tree:
// OFFSHORE_SCAN_DIR=<dir> [OFFSHORE_SCAN_TIER=<scratch dir>]
// [OFFSHORE_SCAN_WORKERS=n] go test -run TestScanThroughput -v.
func TestScanThroughput(t *testing.T) {
	dir := os.Getenv("OFFSHORE_SCAN_DIR")
	if dir == "" {
		t.Skip("OFFSHORE_SCAN_DIR not set")
	}
	opt := ScanOptions{Sink: func(Key, []byte) error { return nil }}
	opt.Workers, _ = strconv.Atoi(os.Getenv("OFFSHORE_SCAN_WORKERS"))
	if td := os.Getenv("OFFSHORE_SCAN_TIER"); td != "" {
		l, err := OpenLocal(LocalOptions{Dir: td})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		opt.Sink = func(k Key, b []byte) error { return l.Put(ctx, k, b, Dirty) }
		opt.RangeSink = func(k Key, b []byte, r Range) error { return PutPage(ctx, l, k, b, r, Dirty) }
	}
	for i := 0; i < 2; i++ {
		begin := time.Now()
		_, st, err := Scan(dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		d := time.Since(begin)
		t.Logf("run %d: %d files %.0f MB in %v: %.0f MB/s", i, st.Hashed, float64(st.Read)/1e6, d.Round(time.Millisecond), float64(st.Read)/1e6/d.Seconds())
	}
}
