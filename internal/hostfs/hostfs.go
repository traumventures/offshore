// Package hostfs inspects the file system holding a directory (type,
// mount options, reflink support) and writes files crash consistently.
//
// The worker's cache medium must be xfs with reflink on cloud machines:
// session snapshots clone changed files (FICLONE) and the rename-based
// commits rely on its allocation behaviour. Hosts the user owns
// (local-docker, static) may run on ext4 and other file systems with a
// warning. Every probe is an injectable function so the policy is unit
// tested without real mounts.
package hostfs

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Statfs magic numbers of the file systems the policy names.
const (
	MagicXFS     = 0x58465342
	MagicExt4    = 0xEF53 // ext2, ext3 and ext4 share it
	MagicBtrfs   = 0x9123683E
	MagicTmpfs   = 0x01021994
	MagicOverlay = 0x794C7630
	MagicZFS     = 0x2FC12FC1
)

var magicNames = map[int64]string{
	MagicXFS: "xfs", MagicExt4: "ext4", MagicBtrfs: "btrfs", MagicTmpfs: "tmpfs",
	MagicOverlay: "overlay", MagicZFS: "zfs",
}

// TypeOfMagic names a statfs magic ("" if unknown).
func TypeOfMagic(m int64) string { return magicNames[m] }

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	MountPoint string
	FSType     string
	Source     string
	// Options are the per-mount and the super block options together
	// (e.g. "rw", "noatime", "noauto_da_alloc").
	Options []string
}

// HasOption reports whether the mount has option o (or o=value).
func (m Mount) HasOption(o string) bool {
	for _, x := range m.Options {
		if x == o || strings.HasPrefix(x, o+"=") {
			return true
		}
	}
	return false
}

// ParseMountinfo parses /proc/self/mountinfo
// ("id parent maj:min root mountpoint opts [optional...] - fstype source superopts").
func ParseMountinfo(r io.Reader) []Mount {
	var out []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(f) {
			continue
		}
		m := Mount{MountPoint: unescape(f[4]), FSType: f[sep+1], Source: unescape(f[sep+2])}
		m.Options = strings.Split(f[5], ",")
		if sep+3 < len(f) {
			m.Options = append(m.Options, strings.Split(f[sep+3], ",")...)
		}
		out = append(out, m)
	}
	return out
}

// unescape decodes the \NNN escapes of mountinfo fields.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MountOf returns the mount containing path p: the last (most recently
// mounted) entry with the longest mount point that is a prefix of p.
func MountOf(mounts []Mount, p string) (Mount, bool) {
	p = filepath.Clean(p)
	best, found := Mount{}, false
	for _, m := range mounts {
		mp := filepath.Clean(m.MountPoint)
		if !(p == mp || mp == "/" || strings.HasPrefix(p, mp+"/")) {
			continue
		}
		if !found || len(mp) >= len(filepath.Clean(best.MountPoint)) {
			best, found = m, true
		}
	}
	return best, found
}

// Info describes the file system holding a directory.
type Info struct {
	Path  string
	Type  string // "xfs", "ext4", ... (statfs magic, refined by mountinfo)
	Magic int64
	// Mount is the covering mount (zero if mountinfo was unreadable).
	Mount Mount
	// Reflink reports whether files below Path can be cloned (FICLONE).
	Reflink bool
}

// NoAutoDaAlloc reports an ext4 mount with noauto_da_alloc: ext4 then
// no longer flushes a file renamed over another before the rename is
// durable, so a crash can leave a replaced file empty unless the writer
// fsyncs it first.
func (i Info) NoAutoDaAlloc() bool {
	return i.Type == "ext4" && i.Mount.HasOption("noauto_da_alloc")
}

// Probes are the system calls Inspect uses (nil fields: the real ones).
type Probes struct {
	Statfs    func(path string, st *unix.Statfs_t) error
	Mountinfo func() (io.ReadCloser, error)
	Reflink   func(dir string) bool
}

func (p Probes) statfs(path string, st *unix.Statfs_t) error {
	if p.Statfs != nil {
		return p.Statfs(path, st)
	}
	return unix.Statfs(path, st)
}

func (p Probes) mountinfo() (io.ReadCloser, error) {
	if p.Mountinfo != nil {
		return p.Mountinfo()
	}
	return os.Open("/proc/self/mountinfo")
}

func (p Probes) reflink(dir string) bool {
	if p.Reflink != nil {
		return p.Reflink(dir)
	}
	return ProbeReflink(dir)
}

// Inspect describes the file system holding dir. withReflink also
// probes cloning, which writes two small temporary files into dir.
func (p Probes) Inspect(dir string, withReflink bool) (Info, error) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	info := Info{Path: dir}
	var st unix.Statfs_t
	if err := p.statfs(dir, &st); err != nil {
		return info, fmt.Errorf("statfs %s: %w", dir, err)
	}
	info.Magic = int64(st.Type)
	info.Type = TypeOfMagic(info.Magic)
	if rc, err := p.mountinfo(); err == nil {
		ms := ParseMountinfo(rc)
		rc.Close()
		if m, ok := MountOf(ms, dir); ok {
			info.Mount = m
			// mountinfo tells ext2/3/4 apart; it is also the only name of
			// file systems without a known magic.
			if info.Type == "" || info.Type == "ext4" {
				info.Type = m.FSType
			}
		}
	}
	if info.Type == "" {
		info.Type = fmt.Sprintf("0x%x", info.Magic)
	}
	if withReflink {
		info.Reflink = p.reflink(dir)
	}
	return info, nil
}

// Inspect describes the file system holding dir with the real probes.
func Inspect(dir string, withReflink bool) (Info, error) {
	return Probes{}.Inspect(dir, withReflink)
}

// ProbeReflink reports whether files below dir can be cloned.
func ProbeReflink(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	a, err := os.CreateTemp(dir, ".reflink-")
	if err != nil {
		return false
	}
	defer os.Remove(a.Name())
	defer a.Close()
	a.Write([]byte("x"))
	b, err := os.CreateTemp(dir, ".reflink-")
	if err != nil {
		return false
	}
	defer os.Remove(b.Name())
	defer b.Close()
	return unix.IoctlFileClone(int(b.Fd()), int(a.Fd())) == nil
}

// Policy is what a cache medium must provide.
type Policy string

const (
	// RequireXFS: xfs with reflink (cloud machines). Anything else is a
	// problem reported loudly.
	RequireXFS Policy = "xfs-reflink"
	// Lenient: any file system; non-xfs or no reflink is a warning
	// (local-docker, static hosts).
	Lenient Policy = ""
)

// ParsePolicy reads a policy name ("" or "xfs-reflink").
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case RequireXFS, Lenient:
		return Policy(s), nil
	}
	return Lenient, fmt.Errorf("unknown cache file system policy %q (want %q or empty)", s, RequireXFS)
}

// Verdict is the outcome of checking a medium against a policy.
type Verdict struct {
	// OK: the policy is met (warnings may remain).
	OK bool
	// Problems break the policy; Warnings do not.
	Problems []string
	Warnings []string
}

// Check holds info against policy.
func Check(info Info, policy Policy) Verdict {
	var issues []string
	if info.Type != "xfs" {
		issues = append(issues, fmt.Sprintf("%s is on %s, not xfs", info.Path, info.Type))
	}
	if !info.Reflink {
		issues = append(issues, fmt.Sprintf("%s does not support reflinks (session snapshots copy instead of cloning, large ones are skipped)", info.Path))
	}
	v := Verdict{OK: true}
	if policy == RequireXFS && len(issues) > 0 {
		v.OK, v.Problems = false, issues
	} else {
		v.Warnings = issues
	}
	if info.NoAutoDaAlloc() {
		v.Warnings = append(v.Warnings, fmt.Sprintf("%s is ext4 mounted with noauto_da_alloc: files replaced by rename are not flushed before the rename, a crash can leave unsynced cache files empty (offshore fsyncs its commit metadata; remount without noauto_da_alloc)", info.Path))
	}
	return v
}

// WriteFileSync writes path crash consistently: a temporary file in the
// same directory is written and fsynced, renamed over path, and the
// directory fsynced so the rename itself is durable. After a crash path
// holds either the old or the new content, on any file system and with
// any mount options (ext4 noauto_da_alloc, xfs).
func WriteFileSync(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs a directory (making renames and creations in it durable).
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SyncFS flushes the whole file system holding dir (syncfs(2)): used
// before a commit marker that vouches for many files written without
// individual fsyncs (snapshot captures).
func SyncFS(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Syncfs(int(d.Fd()))
}
