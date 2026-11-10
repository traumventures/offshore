// Package cas is a local content addressed store keyed by BLAKE3 hex hashes.
package cas

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/traumventures/cloudsourced/internal/manifest"
)

// Store keeps blobs below Dir.
type Store struct {
	Dir string
}

// New creates the store directory.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// Path returns the location of a blob (which may not exist).
func (s *Store) Path(hash string) string {
	if len(hash) < 4 {
		return filepath.Join(s.Dir, "invalid", hash)
	}
	return filepath.Join(s.Dir, hash[:2], hash)
}

// Has reports whether the blob exists.
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.Path(hash))
	return err == nil
}

// Put stores the content of r, verifying it against hash. It returns the
// final path.
func (s *Store) Put(hash string, r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.Dir, "tmp"), "blob-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	got, _, err := manifest.HashReader(io.TeeReader(r, tmp))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if got != hash {
		return "", fmt.Errorf("cas: content hash mismatch: want %s, got %s", hash, got)
	}
	return s.commit(tmp.Name(), hash)
}

// PutFile hashes and stores a file, returning its hash.
func (s *Store) PutFile(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	return s.Ingest(in)
}

// Ingest stores the content of r, whose hash is not known in advance, and
// returns its hash.
func (s *Store) Ingest(r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.Dir, "tmp"), "blob-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	hash, _, err := manifest.HashReader(io.TeeReader(r, tmp))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if s.Has(hash) {
		return hash, nil
	}
	_, err = s.commit(tmp.Name(), hash)
	return hash, err
}

func (s *Store) commit(tmp, hash string) (string, error) {
	dst := s.Path(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}
