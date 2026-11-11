package worker

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/traumventures/cloudsourced/internal/proto"
)

func TestSendBlobCompressed(t *testing.T) {
	data := strings.Repeat("test binary bytes ", 50000)
	for _, compress := range []string{"", proto.CompressZstd} {
		a, b := net.Pipe()
		go func() {
			sendBlob(proto.NewConn(a), "h", int64(len(data)), strings.NewReader(data), compress)
			a.Close()
		}()
		pc := proto.NewConn(b)
		var hdr proto.BlobHeader
		if err := pc.Expect(proto.TBlobHeader, &hdr); err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := pc.ReadData(&wire); err != nil {
			t.Fatal(err)
		}
		got := wire.Bytes()
		if compress != "" {
			if wire.Len() >= len(data)/10 {
				t.Fatalf("not compressed: %d bytes", wire.Len())
			}
			dec, _ := zstd.NewReader(&wire)
			got, _ = io.ReadAll(dec)
			dec.Close()
		}
		if string(got) != data || hdr.Size != int64(len(data)) {
			t.Fatalf("%q: content or size mismatch (%d)", compress, hdr.Size)
		}
		b.Close()
	}
}
