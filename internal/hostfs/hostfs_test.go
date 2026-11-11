package hostfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const mountinfo = `22 1 259:1 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p1 rw,errors=remount-ro
35 22 0:31 / /run rw,nosuid,nodev - tmpfs tmpfs rw,size=1600m
40 22 259:3 / /var/lib/offshore/cache rw,noatime shared:20 - xfs /dev/nvme1n1 rw,attr2,inode64,logbufs=8,noquota
41 22 259:4 / /srv/ext\040disk rw,noatime - ext4 /dev/sdb1 rw,noauto_da_alloc,data=ordered
42 22 259:5 / /srv/ext3 rw - ext3 /dev/sdc1 rw
50 40 259:3 /docker /var/lib/offshore/cache/inner rw shared:21 master:20 - xfs /dev/nvme1n1 rw
`

func TestParseMountinfo(t *testing.T) {
	ms := ParseMountinfo(strings.NewReader(mountinfo + "garbage line\n"))
	if len(ms) != 6 {
		t.Fatalf("%d mounts, want 6: %+v", len(ms), ms)
	}
	if ms[3].MountPoint != "/srv/ext disk" || ms[3].FSType != "ext4" || ms[3].Source != "/dev/sdb1" {
		t.Fatalf("escaped mount: %+v", ms[3])
	}
	if !ms[3].HasOption("noauto_da_alloc") || !ms[3].HasOption("noatime") || !ms[3].HasOption("data") {
		t.Fatalf("options (mount and super): %v", ms[3].Options)
	}
	if ms[0].HasOption("noauto_da_alloc") {
		t.Fatal("root has no noauto_da_alloc")
	}
	// Optional fields (shared:, master:) do not shift the fstype.
	if ms[5].FSType != "xfs" || ms[5].MountPoint != "/var/lib/offshore/cache/inner" {
		t.Fatalf("optional fields: %+v", ms[5])
	}
}

func TestMountOf(t *testing.T) {
	ms := ParseMountinfo(strings.NewReader(mountinfo))
	for p, want := range map[string]string{
		"/var/lib/offshore/cache/anyfs":       "/var/lib/offshore/cache",
		"/var/lib/offshore/cache":             "/var/lib/offshore/cache",
		"/var/lib/offshore/cachex":            "/",
		"/var/lib/offshore/cache/inner/a/b":   "/var/lib/offshore/cache/inner",
		"/srv/ext disk/x":                     "/srv/ext disk",
		"/home/user/.cache/offshore/../cache": "/",
	} {
		m, ok := MountOf(ms, p)
		if !ok || m.MountPoint != want {
			t.Errorf("MountOf(%s) = %q, want %q", p, m.MountPoint, want)
		}
	}
}

// fakeProbes inspect a fake host with the given statfs magic per path
// prefix and the mountinfo above.
func fakeProbes(magic int64, reflink bool) Probes {
	return Probes{
		Statfs: func(path string, st *unix.Statfs_t) error {
			if strings.HasPrefix(path, "/missing") {
				return errors.New("no such file")
			}
			st.Type = magic
			return nil
		},
		Mountinfo: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(mountinfo)), nil },
		Reflink:   func(string) bool { return reflink },
	}
}

func TestInspectAndCheck(t *testing.T) {
	cases := []struct {
		name     string
		dir      string
		magic    int64
		reflink  bool
		policy   Policy
		wantType string
		ok       bool
		problems int
		warnings int
		noAutoDa bool
	}{
		{"xfs reflink strict", "/var/lib/offshore/cache/anyfs", MagicXFS, true, RequireXFS, "xfs", true, 0, 0, false},
		{"xfs without reflink strict", "/var/lib/offshore/cache", MagicXFS, false, RequireXFS, "xfs", false, 1, 0, false},
		{"ext4 strict", "/home/u", MagicExt4, false, RequireXFS, "ext4", false, 2, 0, false},
		{"ext4 lenient", "/home/u", MagicExt4, false, Lenient, "ext4", true, 0, 2, false},
		{"ext4 noauto_da_alloc", "/srv/ext disk/c", MagicExt4, false, Lenient, "ext4", true, 0, 3, true},
		{"ext3 refined by mountinfo", "/srv/ext3/c", MagicExt4, false, Lenient, "ext3", true, 0, 2, false},
		{"btrfs reflink lenient", "/home/u", MagicBtrfs, true, Lenient, "btrfs", true, 0, 1, false},
		{"unknown magic named by mountinfo", "/run/x", 0x1234, false, Lenient, "tmpfs", true, 0, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := fakeProbes(c.magic, c.reflink).Inspect(c.dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if info.Type != c.wantType || info.Reflink != c.reflink || info.NoAutoDaAlloc() != c.noAutoDa {
				t.Fatalf("info %+v", info)
			}
			v := Check(info, c.policy)
			if v.OK != c.ok || len(v.Problems) != c.problems || len(v.Warnings) != c.warnings {
				t.Fatalf("verdict %+v", v)
			}
		})
	}
	if _, err := fakeProbes(MagicXFS, true).Inspect("/missing/dir", true); err == nil {
		t.Fatal("statfs error not reported")
	}
	// Unreadable mountinfo: the statfs type alone.
	p := fakeProbes(MagicXFS, true)
	p.Mountinfo = func() (io.ReadCloser, error) { return nil, errors.New("no proc") }
	if info, err := p.Inspect("/x", false); err != nil || info.Type != "xfs" || info.Reflink {
		t.Fatalf("%+v %v", info, err)
	}
	p.Statfs = func(_ string, st *unix.Statfs_t) error { st.Type = 0x42; return nil }
	if info, _ := p.Inspect("/x", false); info.Type != "0x42" {
		t.Fatalf("unknown type %q", info.Type)
	}
}

func TestParsePolicy(t *testing.T) {
	for in, want := range map[string]Policy{"": Lenient, "xfs-reflink": RequireXFS} {
		if got, err := ParsePolicy(in); err != nil || got != want {
			t.Errorf("ParsePolicy(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParsePolicy("ext4"); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestInspectReal(t *testing.T) {
	dir := t.TempDir()
	info, err := Inspect(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type == "" || info.Mount.MountPoint == "" {
		t.Fatalf("%+v", info)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("reflink probe left files: %v", ents)
	}
}

func TestWriteFileSync(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta.json")
	if err := WriteFileSync(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileSync(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("%q", b)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left")
	}
	if err := WriteFileSync(filepath.Join(t.TempDir(), "no", "dir"), nil, 0o644); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := SyncFS(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
}
