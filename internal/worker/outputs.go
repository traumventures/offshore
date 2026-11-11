package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
)

// serveOutputs answers one output FUSE request (directory listing or whole
// file read) from the most recent build slot of a repository. Access is
// confined to the slot's data directory with os.Root, so symlinks inside
// build outputs cannot escape it.
func (s *Worker) serveOutputs(ctx context.Context, pc *proto.Conn) error {
	t, payload, err := pc.Read()
	if err != nil {
		return err
	}
	switch t {
	case proto.TOutList:
		var req proto.OutList
		if err := proto.Decode(t, payload, proto.TOutList, &req); err != nil {
			return err
		}
		dir, err := s.outputsList(req)
		if err != nil {
			return pc.SendError(stageOf(err), err)
		}
		return pc.Send(proto.TOutDir, dir)
	case proto.TOutRead:
		var req proto.OutRead
		if err := proto.Decode(t, payload, proto.TOutRead, &req); err != nil {
			return err
		}
		return s.outputsRead(pc, req)
	}
	return pc.SendError("protocol", fmt.Errorf("unexpected frame %d on outputs stream", t))
}

var errStale = errors.New("output changed since it was listed")

func stageOf(err error) string {
	switch {
	case errors.Is(err, errStale):
		return "stale"
	case errors.Is(err, fs.ErrNotExist):
		return "notfound"
	}
	return "outputs"
}

// cleanRel validates a slash separated path relative to the repository.
func cleanRel(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	c := path.Clean(p)
	if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}

func (s *Worker) outputsRoot(repoID string) (*os.Root, string, error) {
	if repoID == "" || strings.ContainsAny(repoID, "/.") {
		return nil, "", fmt.Errorf("invalid repository id %q", repoID)
	}
	slot, err := s.slots.latest(repoID, s.cfg.BuildTTL)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", fs.ErrNotExist, err)
	}
	s.mountSlot(slot, repoID)
	r, err := os.OpenRoot(filepath.Join(slot, "data"))
	if err != nil {
		return nil, "", err
	}
	return r, filepath.Base(slot), nil
}

func (s *Worker) outputsList(req proto.OutList) (proto.OutDir, error) {
	rel, err := cleanRel(req.Dir)
	if err != nil {
		return proto.OutDir{}, err
	}
	r, slot, err := s.outputsRoot(req.RepoID)
	if err != nil {
		return proto.OutDir{}, err
	}
	defer r.Close()
	f, err := r.Open(filepath.FromSlash(rel))
	if err != nil {
		return proto.OutDir{}, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return proto.OutDir{}, err
	}
	out := proto.OutDir{Slot: slot, Entries: make([]proto.OutEntry, 0, len(des))}
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			continue // removed concurrently
		}
		e := proto.OutEntry{Name: de.Name(), Mode: uint32(fi.Mode().Perm()), MtimeNs: fi.ModTime().UnixNano()}
		switch {
		case fi.Mode().IsRegular():
			e.Kind, e.Size = manifest.File, fi.Size()
		case fi.IsDir():
			e.Kind = manifest.Dir
		case fi.Mode()&os.ModeSymlink != 0:
			e.Kind = manifest.Symlink
			e.Target, _ = r.Readlink(filepath.Join(filepath.FromSlash(rel), de.Name()))
		default:
			continue // sockets, fifos: not outputs
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

func (s *Worker) outputsRead(pc *proto.Conn, req proto.OutRead) error {
	rel, err := cleanRel(req.Path)
	if err != nil || rel == "." {
		return pc.SendError("outputs", fmt.Errorf("invalid path %q", req.Path))
	}
	r, _, err := s.outputsRoot(req.RepoID)
	if err != nil {
		return pc.SendError(stageOf(err), err)
	}
	defer r.Close()
	f, err := r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return pc.SendError(stageOf(err), err)
	}
	defer f.Close()
	same := func() bool {
		fi, err := f.Stat()
		return err == nil && fi.Mode().IsRegular() && fi.Size() == req.Size && fi.ModTime().UnixNano() == req.MtimeNs
	}
	if !same() {
		return pc.SendError("stale", errStale)
	}
	if err := pc.Send(proto.TBlobHeader, proto.BlobHeader{Size: req.Size}); err != nil {
		return err
	}
	if err := pc.SendData(io.LimitReader(f, req.Size)); err != nil {
		return err
	}
	s.countBytes(float64(req.Size), transferOutput)
	if !same() {
		return pc.SendError("stale", errStale)
	}
	return pc.Write(proto.TDataEnd, nil)
}
