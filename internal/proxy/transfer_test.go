package proxy

import (
	"bytes"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
)

func TestReceiveBlobsZstd(t *testing.T) {
	t.Setenv("CLOUDSOURCED_STATE_DIR", t.TempDir())
	data := []byte(strings.Repeat("ELF test binary ", 10000))
	hash := manifest.HashBytes(data)
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		srv := proto.NewConn(b)
		var need proto.Need
		if srv.Expect(proto.TNeed, &need) != nil || need.Compress != proto.CompressZstd {
			b.Close()
			return
		}
		var z bytes.Buffer
		enc, _ := zstd.NewWriter(&z)
		enc.Write(data)
		enc.Close()
		srv.SendBlob(hash, int64(len(data)), &z)
	}()
	staged, cleanup, err := receiveBlobs(proto.NewConn(a), []string{hash}, proto.CompressZstd)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(staged[hash])
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("staged content wrong: %v", err)
	}
}
