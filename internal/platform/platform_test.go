package platform

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestParseRoundTrip(t *testing.T) {
	for _, s := range []string{"linux/amd64", "linux/arm64/glibc", "linux/amd64/glibc-2.39", "linux/arm64/musl-1.2", "darwin/arm64"} {
		p, err := Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if p.String() != s {
			t.Errorf("%s -> %s", s, p)
		}
	}
	if p, _ := Parse("linux/x86_64"); p.Arch != "amd64" {
		t.Errorf("x86_64 not normalised: %v", p)
	}
	for _, bad := range []string{"", "linux", "linux/amd64/bionic", "linux/amd64/glibc-x", "a/b/c/d"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestCacheKeyIgnoresLibcVersion(t *testing.T) {
	a, _ := Parse("linux/amd64/glibc-2.36")
	b, _ := Parse("linux/amd64/glibc-2.39")
	if a.CacheKey() != b.CacheKey() || a.CacheKey() != "linux/amd64/glibc" {
		t.Fatalf("%s vs %s", a.CacheKey(), b.CacheKey())
	}
}

func TestDetect(t *testing.T) {
	none := func(string) string { return "" }
	glibc := func(name string, args ...string) (string, error) {
		if name == "getconf" {
			return "glibc 2.39\n", nil
		}
		return "", errors.New("no")
	}
	musl := func(name string, args ...string) (string, error) {
		if name == "ldd" {
			return "musl libc (x86_64)\nVersion 1.2.4\n", errors.New("exit 1")
		}
		return "", errors.New("no")
	}
	if runtime.GOOS == "linux" {
		if p := detect(none, glibc); p.Libc != Glibc || p.LibcVersion != (Version{2, 39}) {
			t.Errorf("glibc: %v", p)
		}
		if p := detect(none, musl); p.Libc != Musl || p.LibcVersion != (Version{1, 2}) {
			t.Errorf("musl: %v", p)
		}
	}
	env := func(k string) string {
		if k == EnvVar {
			return "linux/arm64/glibc-2.31"
		}
		return ""
	}
	if p := detect(env, glibc); p.String() != "linux/arm64/glibc-2.31" {
		t.Errorf("override: %v", p)
	}
}

func TestCanRun(t *testing.T) {
	host := Host{Platform: Platform{OS: "linux", Arch: "amd64", Libc: Glibc, LibcVersion: Version{2, 36}},
		Libs: map[string]bool{"libc.so.6": true}, Exists: func(p string) bool { return p == "/lib64/ld-linux-x86-64.so.2" || p == "/opt/lib/libfoo.so" }}
	bin := func(arch, libc string, v Version, libs ...string) Binary {
		return Binary{Path: "/t/x", Platform: Platform{OS: "linux", Arch: arch, Libc: libc, LibcVersion: v},
			Interp: "/lib64/ld-linux-x86-64.so.2", Libs: libs, Search: []string{"/opt/lib"}}
	}
	cases := []struct {
		b    Binary
		want bool
	}{
		{bin("amd64", Glibc, Version{2, 34}, "libc.so.6"), true},
		{bin("amd64", Glibc, Version{2, 39}), false},
		{bin("arm64", Glibc, Version{2, 34}), false},
		{bin("amd64", Musl, Version{}), false},
		{bin("amd64", Glibc, Version{2, 17}, "libfoo.so"), true},
		{bin("amd64", Glibc, Version{2, 17}, "libpq.so.5"), false},
		{Binary{Path: "/t/s", Platform: Platform{OS: "linux", Arch: "amd64"}}, true}, // static
		{Binary{Path: "/t/s", Platform: Platform{OS: "linux", Arch: "amd64"}, Interp: "/lib/ld-musl-x86_64.so.1"}, false},
	}
	for i, c := range cases {
		why := host.CanRun(c.b)
		if (why == "") != c.want {
			t.Errorf("case %d: why=%q want ok=%v", i, why, c.want)
		}
	}
}

func TestReadBinarySelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ELF")
	}
	exe, _ := os.Executable()
	b, err := ReadBinary(exe)
	if err != nil {
		t.Fatal(err)
	}
	if b.Platform.Arch != runtime.GOARCH {
		t.Fatalf("arch %s", b.Platform.Arch)
	}
	if why := CheckRun([]string{exe}); why != "" {
		t.Fatalf("own test binary cannot run: %s", why)
	}
}

func TestParseLdconfig(t *testing.T) {
	libs := ParseLdconfig("1 libs found in cache\n\tlibc.so.6 (libc6,x86-64) => /lib/libc.so.6\n")
	if !libs["libc.so.6"] || len(libs) != 1 {
		t.Fatal(libs)
	}
}
