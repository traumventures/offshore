package manifest

import (
	"bufio"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// Mode is the projection mode of a path.
type Mode int

const (
	ModeWatch Mode = iota
	ModeImmutable
	ModeHidden
)

// Rule maps glob patterns (doublestar syntax, relative to the root) to a mode.
type Rule struct {
	Patterns []string
	Mode     Mode
}

// Rules decides the projection mode of paths. Later rules win.
type Rules struct {
	Default   Mode
	Gitignore bool
	Rules     []Rule
	// Hidden are directories (relative paths) that are never projected,
	// e.g. ".git" and worker-local build directories.
	Hidden []string
}

// Scanner maintains the tree of a root directory.
type Scanner struct {
	Root  string
	Rules Rules

	mu      sync.Mutex
	ignores map[string][]gitignore.Pattern // dir -> patterns from its .gitignore
}

// NewScanner creates a scanner for root.
func NewScanner(root string, rules Rules) *Scanner {
	return &Scanner{Root: root, Rules: rules, ignores: map[string][]gitignore.Pattern{}}
}

// ModeOf returns the mode of a relative path.
func (s *Scanner) ModeOf(rel string, isDir bool) Mode {
	if rel == "" {
		return s.Rules.Default
	}
	for _, h := range s.Rules.Hidden {
		if rel == h || strings.HasPrefix(rel, h+"/") {
			return ModeHidden
		}
	}
	if path.Base(rel) == ".git" {
		return ModeHidden
	}
	mode := s.Rules.Default
	if s.Rules.Gitignore && s.ignored(rel, isDir) {
		mode = ModeHidden
	}
	for _, r := range s.Rules.Rules {
		for _, p := range r.Patterns {
			if matchRule(p, rel, isDir) {
				mode = r.Mode
			}
		}
	}
	return mode
}

func matchRule(pattern, rel string, isDir bool) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	if ok, _ := doublestar.Match(pattern, rel); ok {
		return true
	}
	// "vendor/**" should also match the directory "vendor" itself.
	if isDir && strings.HasSuffix(pattern, "/**") {
		if ok, _ := doublestar.Match(strings.TrimSuffix(pattern, "/**"), rel); ok {
			return true
		}
	}
	return false
}

func (s *Scanner) ignored(rel string, isDir bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := strings.Split(rel, "/")
	// Collect patterns from the root down to the parent directory, in order,
	// so deeper .gitignore files take precedence.
	var ps []gitignore.Pattern
	ps = append(ps, s.ignores[""]...)
	for i := 1; i < len(parts); i++ {
		ps = append(ps, s.ignores[strings.Join(parts[:i], "/")]...)
	}
	if len(ps) == 0 {
		return false
	}
	// Any ignored ancestor directory hides the path as well.
	m := gitignore.NewMatcher(ps)
	for i := 1; i <= len(parts); i++ {
		dir := i < len(parts) || isDir
		if m.Match(parts[:i], dir) {
			return true
		}
	}
	return false
}

func (s *Scanner) loadIgnore(relDir string) {
	if !s.Rules.Gitignore {
		return
	}
	var files []string
	files = append(files, filepath.Join(s.Root, filepath.FromSlash(relDir), ".gitignore"))
	if relDir == "" {
		files = append(files, filepath.Join(s.Root, ".git", "info", "exclude"))
	}
	var domain []string
	if relDir != "" {
		domain = strings.Split(relDir, "/")
	}
	var ps []gitignore.Pattern
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue
			}
			ps = append(ps, gitignore.ParsePattern(line, domain))
		}
		fh.Close()
	}
	s.mu.Lock()
	if len(ps) == 0 {
		delete(s.ignores, relDir)
	} else {
		s.ignores[relDir] = ps
	}
	s.mu.Unlock()
}

// Scan walks the whole root and returns a fresh tree. Hashes of files whose
// size, mtime and mode are unchanged compared to prev are reused.
func (s *Scanner) Scan(prev Tree) (Tree, error) {
	out := Tree{}
	s.mu.Lock()
	s.ignores = map[string][]gitignore.Pattern{}
	s.mu.Unlock()
	if err := s.scanDir("", prev, out, true); err != nil {
		return nil, err
	}
	return out, nil
}

// Rescan updates tree t for the given directories (relative paths). Each
// directory is re-read; new subdirectories are scanned recursively. It
// returns the resulting delta (without version).
func (s *Scanner) Rescan(t Tree, dirs []string) (Delta, error) {
	next := t.Clone()
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.Trim(d, "/")
		if d == "." {
			d = ""
		}
		// Walk up to the closest existing, visible directory.
		for d != "" {
			if e, ok := next[d]; ok && e.Kind == Dir {
				if fi, err := os.Lstat(s.abs(d)); err == nil && fi.IsDir() {
					break
				}
			}
			d = parentOf(d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := s.rescanDir(d, t, next); err != nil {
			return Delta{}, err
		}
	}
	return Diff(t, next), nil
}

func parentOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

func (s *Scanner) abs(rel string) string {
	return filepath.Join(s.Root, filepath.FromSlash(rel))
}

// rescanDir re-reads one directory non-recursively, scanning new or
// replaced subdirectories recursively.
func (s *Scanner) rescanDir(rel string, prev, next Tree) error {
	s.loadIgnore(rel)
	children := map[string]bool{}
	des, err := os.ReadDir(s.abs(rel))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, de := range des {
		childRel := join(rel, de.Name())
		children[childRel] = true
		e, ok, err := s.entry(childRel, prev)
		if err != nil {
			return err
		}
		if !ok {
			next.DeleteSubtree(childRel)
			continue
		}
		old, existed := next[childRel]
		if e.Kind == Dir {
			next[childRel] = e
			if !existed || old.Kind != Dir {
				if existed {
					next.DeleteSubtree(childRel)
					next[childRel] = e
				}
				if err := s.scanDir(childRel, prev, next, true); err != nil {
					return err
				}
			}
			continue
		}
		if existed && old.Kind == Dir {
			next.DeleteSubtree(childRel)
		}
		next[childRel] = e
	}
	// Remove children that disappeared.
	for p := range next {
		if parentOf(p) == rel && p != rel && !children[p] {
			next.DeleteSubtree(p)
		}
	}
	return nil
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (s *Scanner) scanDir(rel string, prev, out Tree, recursive bool) error {
	s.loadIgnore(rel)
	des, err := os.ReadDir(s.abs(rel))
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	for _, de := range des {
		childRel := join(rel, de.Name())
		e, ok, err := s.entry(childRel, prev)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		out[childRel] = e
		if e.Kind == Dir && recursive {
			if err := s.scanDir(childRel, prev, out, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// entry stats one path. ok is false when the path is hidden or vanished.
func (s *Scanner) entry(rel string, prev Tree) (Entry, bool, error) {
	fi, err := os.Lstat(s.abs(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	isDir := fi.IsDir()
	if s.ModeOf(rel, isDir) == ModeHidden {
		return Entry{}, false, nil
	}
	e := Entry{Path: rel, Mode: uint32(fi.Mode().Perm()), MtimeNs: fi.ModTime().UnixNano()}
	switch {
	case isDir:
		e.Kind = Dir
		e.MtimeNs = 0 // directory mtimes are noise for builds
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Kind = Symlink
		e.Target, err = os.Readlink(s.abs(rel))
		if err != nil {
			return Entry{}, false, nil
		}
		e.Size = int64(len(e.Target))
	case fi.Mode().IsRegular():
		e.Kind = File
		e.Size = fi.Size()
		if p, ok := prev[rel]; ok && p.Kind == File && p.Size == e.Size && p.MtimeNs == e.MtimeNs && p.Mode == e.Mode && p.Hash != "" {
			e.Hash = p.Hash
		} else {
			h, n, err := HashFile(s.abs(rel))
			if err != nil {
				if os.IsNotExist(err) || os.IsPermission(err) {
					return Entry{}, false, nil
				}
				return Entry{}, false, err
			}
			e.Hash, e.Size = h, n
		}
	default:
		return Entry{}, false, nil // sockets, devices, fifos are not projected
	}
	return e, true, nil
}
