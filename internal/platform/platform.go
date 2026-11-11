// Package platform describes where binaries run: operating system,
// architecture and C library (with version). The environment image hash,
// cache digests and the local-run checks of tool adapters use this one
// description, so "built remotely, runs here" and "cache entry fits this
// machine" are decided the same way.
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// C libraries.
const (
	Glibc = "glibc"
	Musl  = "musl"
)

// EnvVar overrides the detected local platform ("linux/amd64/glibc-2.39").
const EnvVar = "CLOUDSOURCED_PLATFORM"

// Platform is an execution platform. Libc and LibcVersion are empty when
// unknown (e.g. an image that was not inspected) or not applicable.
type Platform struct {
	OS          string  // GOOS: linux, darwin, ...
	Arch        string  // GOARCH: amd64, arm64, ...
	Libc        string  // glibc, musl or ""
	LibcVersion Version // zero when unknown
}

// Version is a major.minor version.
type Version struct{ Major, Minor int }

// ParseVersion parses "2.39" (further components are ignored).
func ParseVersion(s string) (Version, bool) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		return Version{}, false
	}
	b, _, _ = strings.Cut(b, ".")
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	return Version{x, y}, err1 == nil && err2 == nil
}

// Less orders versions.
func (v Version) Less(w Version) bool {
	return v.Major < w.Major || (v.Major == w.Major && v.Minor < w.Minor)
}

// IsZero reports an unknown version.
func (v Version) IsZero() bool { return v == Version{} }

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// String renders "os/arch[/libc[-version]]", the form Parse reads.
func (p Platform) String() string {
	if p.Arch == "" {
		return p.OS
	}
	s := p.OS + "/" + p.Arch
	if p.Libc != "" {
		s += "/" + p.Libc
		if !p.LibcVersion.IsZero() {
			s += "-" + p.LibcVersion.String()
		}
	}
	return s
}

// Parse reads the String form.
func Parse(s string) (Platform, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, fmt.Errorf("platform %q: want os/arch[/libc[-version]]", s)
	}
	p := Platform{OS: parts[0], Arch: NormalizeArch(parts[1])}
	if len(parts) == 3 {
		libc, ver, hasVer := strings.Cut(parts[2], "-")
		if libc != Glibc && libc != Musl {
			return Platform{}, fmt.Errorf("platform %q: unknown libc %q (glibc, musl)", s, libc)
		}
		p.Libc = libc
		if hasVer {
			v, ok := ParseVersion(ver)
			if !ok {
				return Platform{}, fmt.Errorf("platform %q: bad libc version %q", s, ver)
			}
			p.LibcVersion = v
		}
	}
	return p, nil
}

// NormalizeArch maps common architecture names to GOARCH.
func NormalizeArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "x86-64", "x64":
		return "amd64"
	case "aarch64", "armv8":
		return "arm64"
	}
	return strings.ToLower(a)
}

// Target is the platform of build machines for a configured architecture
// ("" = not fixed by the repository: Arch stays empty). Machines are Linux;
// the C library is the environment image's and unknown until the image is
// inspected.
func Target(arch string) Platform {
	return Platform{OS: "linux", Arch: NormalizeArch(arch)}
}

// CacheKey is the part of the platform that decides whether cached build
// state can be shared: OS, architecture and C library family. The libc
// version is left out so a glibc point release does not cold-start every
// cache (binaries record the symbol versions they need; see CanRun).
func (p Platform) CacheKey() string {
	s := p.OS + "/" + p.Arch
	if p.Libc != "" {
		s += "/" + p.Libc
	}
	return s
}

var (
	localOnce sync.Once
	localVal  Platform
)

// Local is the platform of this process, detected once.
// CLOUDSOURCED_PLATFORM overrides it.
func Local() Platform {
	localOnce.Do(func() { localVal = detect(os.Getenv, runCmd) })
	return localVal
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// detect finds the local platform; run executes the libc probes.
func detect(getenv func(string) string, run func(string, ...string) (string, error)) Platform {
	if s := getenv(EnvVar); s != "" {
		if p, err := Parse(s); err == nil {
			return p
		}
	}
	p := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if p.OS != "linux" {
		return p
	}
	if out, err := run("getconf", "GNU_LIBC_VERSION"); err == nil {
		// "glibc 2.39"
		if f := strings.Fields(out); len(f) == 2 && f[0] == "glibc" {
			p.Libc = Glibc
			p.LibcVersion, _ = ParseVersion(f[1])
			return p
		}
	}
	// musl's ldd prints "musl libc (x86_64)\nVersion 1.2.4" and exits 1.
	if out, _ := run("ldd", "--version"); strings.Contains(out, "musl") {
		p.Libc = Musl
		for _, l := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "Version "); ok {
				p.LibcVersion, _ = ParseVersion(v)
			}
		}
	}
	return p
}
