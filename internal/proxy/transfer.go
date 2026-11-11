package proxy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/traumventures/offshore/internal/cas"
	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/outputfs"
	"github.com/traumventures/offshore/internal/proto"
)

// safeJoin joins root and a relative slash path, refusing escapes.
func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid path %q", rel)
	}
	return filepath.Join(root, clean), nil
}

// localState describes a local path in the terms of Change.BaseHash.
func localState(p string) (state string, hash string) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", ""
	}
	switch {
	case fi.Mode().IsRegular():
		h, _, err := manifest.HashFile(p)
		if err != nil {
			return "?", ""
		}
		return h, h
	case fi.IsDir():
		return "-", "dir"
	case fi.Mode()&os.ModeSymlink != 0:
		t, _ := os.Readlink(p)
		return "-", "link:" + t
	}
	return "?", ""
}

// receiveBlobs requests hashes and stores them in a staging directory.
// compress ("" or proto.CompressZstd) asks for compressed transfer.
func receiveBlobs(pc *proto.Conn, hashes []string, compress string) (map[string]string, func(), error) {
	staged := map[string]string{}
	dir, err := os.MkdirTemp(daemon.StateDir(), "staging-")
	cleanup := func() { os.RemoveAll(dir) }
	if err != nil {
		os.MkdirAll(daemon.StateDir(), 0o700)
		if dir, err = os.MkdirTemp(daemon.StateDir(), "staging-"); err != nil {
			pc.Send(proto.TNeed, proto.Need{})
			return nil, func() {}, err
		}
	}
	if err := pc.Send(proto.TNeed, proto.Need{Hashes: hashes, Compress: compress}); err != nil {
		return nil, cleanup, err
	}
	for range hashes {
		var hdr proto.BlobHeader
		if err := pc.Expect(proto.TBlobHeader, &hdr); err != nil {
			return nil, cleanup, err
		}
		f, err := os.Create(filepath.Join(dir, hdr.Hash))
		if err != nil {
			return nil, cleanup, err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(pc.ReadData(pw)) }()
		var data io.Reader = pr
		var dec *zstd.Decoder
		if compress == proto.CompressZstd {
			if dec, err = zstd.NewReader(pr, zstd.WithDecoderConcurrency(1)); err != nil {
				f.Close()
				return nil, cleanup, err
			}
			data = dec
		}
		got, _, err := manifest.HashReader(io.TeeReader(data, f))
		if dec != nil {
			dec.Close()
			io.Copy(io.Discard, pr) // the end-of-data frame
		}
		f.Close()
		if err != nil {
			return nil, cleanup, err
		}
		if got != hdr.Hash {
			return nil, cleanup, fmt.Errorf("blob %s: content hash mismatch", hdr.Hash)
		}
		staged[hdr.Hash] = f.Name()
	}
	return staged, cleanup, nil
}

// install atomically places a staged blob at dst.
func install(src, dst string, mode os.FileMode, mtime time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".offshore-tmp")
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chmod(tmp, mode)
	if !mtime.IsZero() {
		os.Chtimes(tmp, mtime, mtime)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// receiveWriteback applies remote changes to projected sources. A change is
// applied only if the local file still has the content the remote change
// was based on; otherwise the local version wins and the change is
// reported. It returns the roots that were modified.
func receiveWriteback(pc *proto.Conn, repo *config.Repo) ([]string, error) {
	var wb proto.Writeback
	if err := pc.Expect(proto.TWriteback, &wb); err != nil {
		return nil, err
	}
	roots := map[string]bool{repo.Root: true}
	for _, r := range repo.ExtraRootPaths() {
		roots[r] = true
	}
	type planned struct {
		c   proto.Change
		dst string
	}
	var apply []planned
	var rejected []string
	needSet := map[string]bool{}
	var need []string
	for _, c := range wb.Changes {
		dst, err := safeJoin(c.Root, c.Path)
		if err != nil || !roots[c.Root] {
			rejected = append(rejected, c.Root+"\x00"+c.Path)
			continue
		}
		state, cur := localState(dst)
		target := ""
		switch {
		case c.Deleted:
			target = ""
		case c.Kind == manifest.File:
			target = c.Hash
		case c.Kind == manifest.Dir:
			target = "dir"
		case c.Kind == manifest.Symlink:
			target = "link:" + c.Target
		}
		if cur == target && !(c.Kind == manifest.File && !c.Deleted) {
			continue // already in the desired state
		}
		if cur == target {
			// Same content; only the mode may differ.
			if fi, err := os.Stat(dst); err == nil && uint32(fi.Mode().Perm()) != c.Mode&0o777 {
				os.Chmod(dst, os.FileMode(c.Mode&0o777))
			}
			continue
		}
		if state != c.BaseHash {
			rejected = append(rejected, c.Root+"\x00"+c.Path)
			warn("not writing back %s: changed locally while the remote command ran", dst)
			continue
		}
		apply = append(apply, planned{c, dst})
		if c.Kind == manifest.File && !c.Deleted && !needSet[c.Hash] {
			needSet[c.Hash] = true
			need = append(need, c.Hash)
		}
	}
	staged, cleanup, err := receiveBlobs(pc, need, "")
	defer cleanup()
	if err != nil {
		return nil, err
	}
	touched := map[string]bool{}
	for _, p := range apply {
		c := p.c
		var err error
		switch {
		case c.Deleted:
			if fi, lerr := os.Lstat(p.dst); lerr == nil && fi.IsDir() {
				err = os.Remove(p.dst)
			} else {
				err = os.Remove(p.dst)
			}
			if os.IsNotExist(err) {
				err = nil
			}
		case c.Kind == manifest.Dir:
			err = os.MkdirAll(p.dst, os.FileMode(c.Mode&0o7777)|0o700)
		case c.Kind == manifest.Symlink:
			os.Remove(p.dst)
			err = os.Symlink(c.Target, p.dst)
		case c.Kind == manifest.File:
			err = install(staged[c.Hash], p.dst, os.FileMode(c.Mode&0o7777), time.Time{})
		}
		if err != nil {
			warn("write back %s: %v", p.dst, err)
			rejected = append(rejected, c.Root+"\x00"+c.Path)
			continue
		}
		touched[c.Root] = true
	}
	if err := pc.Send(proto.TWritebackResult, proto.WritebackResult{Rejected: rejected}); err != nil {
		return nil, err
	}
	var out []string
	for r := range touched {
		out = append(out, r)
	}
	return out, nil
}

// receiveOutputs copies changed output files (eager globs) into the local
// repository.
func receiveOutputs(pc *proto.Conn, repoRoot string) error {
	var outs proto.Outputs
	if err := pc.Expect(proto.TOutputs, &outs); err != nil {
		return err
	}
	return ReceiveFiles(pc, repoRoot, outs.Files, "")
}

// ReceiveFiles requests the files whose local content differs and installs
// them below root. Files inside a mounted output directory (output FUSE)
// cannot be written there; they go into the outputs content cache and the
// daemon is told, so the mount serves them without the machine. compress
// ("" or proto.CompressZstd) selects the transfer encoding.
func ReceiveFiles(pc *proto.Conn, root string, files []manifest.Entry, compress string) error {
	type planned struct {
		e   manifest.Entry
		dst string
	}
	var todo []planned
	needSet := map[string]bool{}
	var need []string
	mounts := outputfs.Mounts()
	var store *cas.Store
	var cached []manifest.Entry
	for _, e := range files {
		dst, err := safeJoin(root, e.Path)
		if err != nil {
			continue
		}
		if outputfs.Covering(mounts, dst) != "" {
			if store == nil {
				if store, err = cas.New(daemon.OutputsCASDir()); err != nil {
					return err
				}
			}
			cached = append(cached, e)
			if !store.Has(e.Hash) && !needSet[e.Hash] {
				needSet[e.Hash] = true
				need = append(need, e.Hash)
			}
			continue
		}
		if fi, err := os.Stat(dst); err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
			if h, _, err := manifest.HashFile(dst); err == nil && h == e.Hash {
				continue
			}
		}
		todo = append(todo, planned{e, dst})
		if !needSet[e.Hash] {
			needSet[e.Hash] = true
			need = append(need, e.Hash)
		}
	}
	staged, cleanup, err := receiveBlobs(pc, need, compress)
	defer cleanup()
	if err != nil {
		return err
	}
	for _, p := range todo {
		if err := install(staged[p.e.Hash], p.dst, os.FileMode(p.e.Mode&0o7777), time.Unix(0, p.e.MtimeNs)); err != nil {
			return err
		}
	}
	if len(cached) > 0 {
		for _, e := range cached {
			if src, ok := staged[e.Hash]; ok && !store.Has(e.Hash) {
				if _, err := store.PutFile(src); err != nil {
					return err
				}
			}
		}
		if _, err := daemon.Call(context.Background(), daemon.Request{Op: "outputs-hint", Root: root, Files: cached}, nil); err != nil {
			return fmt.Errorf("outputs: %w", err)
		}
	}
	return nil
}
