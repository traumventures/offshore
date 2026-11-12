package worker

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/traumventures/offshore/internal/anyfs"
)

// sourcesBaseFile, relative to a slot's data directory, is the source
// view the slot's build directories were built against
// (anyfs.MtimeBase): it travels with the slot (snapshots, heads, seeds
// of sibling worktrees, restores on other machines), so a session
// presents unchanged sources with the mtimes the outputs were checked
// against instead of the worktree's checkout times.
const sourcesBaseFile = ".offshore/sources.gz"

// loadSourcesBase reads the slot's source view (nil: none or unreadable).
func loadSourcesBase(data string) *anyfs.MtimeBase {
	f, err := os.Open(filepath.Join(data, sourcesBaseFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := anyfs.ReadMtimeBase(f)
	if err != nil {
		return nil
	}
	return b
}

// saveSourcesBase stores b as the slot's source view unless the slot
// already holds the same files (an unchanged view keeps its older
// capture time: the session built from exactly these files, and the
// slot does not change).
func saveSourcesBase(data string, b *anyfs.MtimeBase) (bool, error) {
	if old := loadSourcesBase(data); old != nil && old.SameFiles(b) {
		return false, nil
	}
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		return false, err
	}
	p := filepath.Join(data, sourcesBaseFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, p)
}
