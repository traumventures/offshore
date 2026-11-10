package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
)

func outputsServer(t *testing.T) (*Server, string) {
	t.Helper()
	sl, err := newSlots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	slot, _, err := sl.lease("repo1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sl.release(slot)
	data := filepath.Join(slot, "data")
	os.MkdirAll(filepath.Join(data, "target", "debug", "deps"), 0o755)
	os.WriteFile(filepath.Join(data, "target", "debug", "app"), []byte("binary"), 0o755)
	os.Symlink("app", filepath.Join(data, "target", "debug", "link"))
	os.Symlink("/etc", filepath.Join(data, "target", "escape"))
	return &Server{cfg: Config{BuildTTL: time.Hour}, slots: sl}, data
}

// call runs one outputs request over an in-memory stream.
func call(t *testing.T, s *Server, typ byte, req any) *proto.Conn {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		s.serveOutputs(context.Background(), proto.NewConn(b))
	}()
	pc := proto.NewConn(a)
	if err := pc.Send(typ, req); err != nil {
		t.Fatal(err)
	}
	return pc
}

func TestServeOutputsListAndRead(t *testing.T) {
	s, data := outputsServer(t)
	var dir proto.OutDir
	if err := call(t, s, proto.TOutList, proto.OutList{RepoID: "repo1", Dir: "target/debug"}).Expect(proto.TOutDir, &dir); err != nil {
		t.Fatal(err)
	}
	got := map[string]proto.OutEntry{}
	for _, e := range dir.Entries {
		got[e.Name] = e
	}
	if e := got["app"]; e.Kind != manifest.File || e.Size != 6 || e.Mode != 0o755 {
		t.Fatalf("app = %+v", e)
	}
	if got["deps"].Kind != manifest.Dir || got["link"].Kind != manifest.Symlink || got["link"].Target != "app" {
		t.Fatalf("entries = %+v", dir.Entries)
	}
	fi, _ := os.Stat(filepath.Join(data, "target", "debug", "app"))
	pc := call(t, s, proto.TOutRead, proto.OutRead{RepoID: "repo1", Path: "target/debug/app", Size: 6, MtimeNs: fi.ModTime().UnixNano()})
	var hdr proto.BlobHeader
	if err := pc.Expect(proto.TBlobHeader, &hdr); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pc.ReadData(&buf); err != nil || buf.String() != "binary" {
		t.Fatalf("read %q, %v", buf.String(), err)
	}
}

func TestServeOutputsErrors(t *testing.T) {
	s, _ := outputsServer(t)
	stage := func(err error) string {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return pe.Stage
		}
		return "?" + err.Error()
	}
	err := call(t, s, proto.TOutList, proto.OutList{RepoID: "repo1", Dir: "target/release"}).Expect(proto.TOutDir, nil)
	if stage(err) != "notfound" {
		t.Fatalf("missing dir: %v", err)
	}
	err = call(t, s, proto.TOutList, proto.OutList{RepoID: "other", Dir: "target"}).Expect(proto.TOutDir, nil)
	if stage(err) != "notfound" {
		t.Fatalf("unknown repo: %v", err)
	}
	err = call(t, s, proto.TOutList, proto.OutList{RepoID: "repo1", Dir: "../.."}).Expect(proto.TOutDir, nil)
	if err == nil {
		t.Fatal("escape with ..")
	}
	err = call(t, s, proto.TOutList, proto.OutList{RepoID: "repo1", Dir: "target/escape"}).Expect(proto.TOutDir, nil)
	if err == nil {
		t.Fatal("escape through a symlink")
	}
	err = call(t, s, proto.TOutRead, proto.OutRead{RepoID: "repo1", Path: "target/debug/app", Size: 6, MtimeNs: 1}).Expect(proto.TBlobHeader, nil)
	if stage(err) != "stale" {
		t.Fatalf("changed file: %v", err)
	}
	err = call(t, s, proto.TOutRead, proto.OutRead{RepoID: "repo1", Path: "target/debug/link", Size: 3}).Expect(proto.TBlobHeader, nil)
	if err == nil {
		t.Fatal("symlink read as file")
	}
}
