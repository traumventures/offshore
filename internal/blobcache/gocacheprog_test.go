package blobcache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// goProgSession runs a protocol session with the given request lines and
// returns the decoded responses (without the initial capabilities).
func goProgSession(t *testing.T, g *GoCacheProg, in string) []goProgResponse {
	t.Helper()
	var out bytes.Buffer
	if err := g.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []goProgResponse
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r goProgResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, r)
	}
	if len(resps) == 0 || len(resps[0].KnownCommands) != 3 {
		t.Fatalf("no capabilities: %+v", resps)
	}
	return resps[1:]
}

func TestGoCacheProgSharedAcrossMachines(t *testing.T) {
	store, _ := NewDirStore(t.TempDir(), time.Hour)
	srv := httptest.NewServer(&Handler{Store: store, Namespace: "env"})
	defer srv.Close()
	body := []byte("compiled package")
	put := `{"ID":1,"Command":"put","ActionID":"qrvM","OutputID":"3q2+7w==","BodySize":16}` + "\n" + EncodeGoProgBody(body) + "\n" +
		`{"ID":2,"Command":"get","ActionID":"qrvM"}` + "\n" + `{"ID":3,"Command":"get","ActionID":"AAAA"}` + "\n" + `{"ID":4,"Command":"close"}` + "\n"
	a := &GoCacheProg{Dir: t.TempDir(), Endpoint: srv.URL + "/"}
	r := goProgSession(t, a, put)
	if len(r) != 4 || r[0].Err != "" || r[0].DiskPath == "" || r[1].Miss || r[1].Size != 16 || !r[2].Miss || r[3].ID != 4 {
		t.Fatalf("responses %+v", r)
	}
	if b, _ := os.ReadFile(r[1].DiskPath); !bytes.Equal(b, body) {
		t.Fatalf("disk path content %q", b)
	}
	// A second machine (empty local dir) finds the entry remotely.
	b := &GoCacheProg{Dir: t.TempDir(), Endpoint: srv.URL + "/"}
	r = goProgSession(t, b, `{"ID":7,"Command":"get","ActionID":"qrvM"}`+"\n")
	if len(r) != 1 || r[0].Miss || r[0].ID != 7 {
		t.Fatalf("remote get %+v", r)
	}
	got, _ := os.ReadFile(r[0].DiskPath)
	if !bytes.Equal(got, body) || !bytes.Equal(r[0].OutputID, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("remote content %q id %x", got, r[0].OutputID)
	}
	// Local only: a miss, no error.
	c := &GoCacheProg{Dir: t.TempDir()}
	if r := goProgSession(t, c, `{"ID":1,"Command":"get","ActionID":"qrvM"}`+"\n"); !r[0].Miss {
		t.Fatal("expected miss")
	}
	_ = io.EOF
}
