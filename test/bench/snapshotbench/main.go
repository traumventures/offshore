// Command snapshotbench saves a directory as a snapshot into a directory
// store and restores it lazily (skeleton first, then the background fill)
// into another directory, printing the timings as JSON. Used by
// test/bench/snapshot.sh.
//
//	snapshotbench save SRC STORE
//	snapshotbench restore STORE DST [first-file]
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/traumventures/cloudsourced/internal/blobcache"
	"github.com/traumventures/cloudsourced/internal/snapshot"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "snapshotbench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: save SRC STORE | restore STORE DST [first-file]")
	}
	ctx := context.Background()
	out := map[string]any{}
	switch args[0] {
	case "save":
		st, err := blobcache.NewDirStore(args[2], blobcache.MaxTTL)
		if err != nil {
			return err
		}
		var prev *snapshot.Manifest
		if m, err := snapshot.Load(ctx, st, 0); err == nil {
			prev = m
		}
		_, s, err := snapshot.Save(ctx, args[1], st, snapshot.Options{Previous: prev})
		if err != nil {
			return err
		}
		out["bytes"], out["files"], out["chunks"], out["new_chunks"], out["seconds"] = s.Bytes, s.Files, s.Chunks, s.NewChunks, s.Seconds
	case "restore":
		st, err := blobcache.NewDirStore(args[1], blobcache.MaxTTL)
		if err != nil {
			return err
		}
		begin := time.Now()
		m, err := snapshot.Load(ctx, st, 0)
		if err != nil {
			return err
		}
		f := snapshot.NewFiller(m, args[2], st, filepath.Join(filepath.Dir(args[2]), "chunks"))
		if err := f.Skeleton(); err != nil {
			return err
		}
		out["skeleton_seconds"] = time.Since(begin).Seconds()
		if len(args) > 3 {
			t := time.Now()
			if err := f.Fill(ctx, args[3]); err != nil {
				return err
			}
			out["first_file_seconds"] = time.Since(t).Seconds()
		}
		if err := f.FillAll(ctx, 16); err != nil {
			return err
		}
		out["fill_seconds"] = time.Since(begin).Seconds()
		out["bytes"], out["chunks_fetched"] = m.Bytes, f.Stats().NewChunks
		f.Close()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
