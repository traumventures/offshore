package platform

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Binaries built remotely come from the machine's environment image. Before
// one runs here, CanRun makes sure this machine can execute it: same OS and
// architecture, the dynamic loader exists, the C library is the same family
// and at least as new as the one it was linked against, and its shared
// libraries are found.

var elfArch = map[elf.Machine]string{
	elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64", elf.EM_386: "386",
	elf.EM_ARM: "arm", elf.EM_RISCV: "riscv64", elf.EM_PPC64: "ppc64le", elf.EM_S390: "s390x",
}

// Binary is what an executable needs from the platform that runs it.
type Binary struct {
	Path string
	// Platform: OS and Arch of the file; Libc from its dynamic loader or
	// symbol versions; LibcVersion the newest GLIBC_x.y symbol it imports.
	Platform Platform
	Interp   string   // dynamic loader, "" for static binaries
	Libs     []string // DT_NEEDED sonames
	Search   []string // extra library directories (RPATH/RUNPATH, $ORIGIN, its parent)
}

// ReadBinary inspects an ELF executable.
func ReadBinary(path string) (Binary, error) {
	f, err := elf.Open(path)
	if err != nil {
		return Binary{}, err
	}
	defer f.Close()
	b := Binary{Path: path, Platform: Platform{OS: "linux", Arch: elfArch[f.Machine]}}
	if b.Platform.Arch == "" {
		b.Platform.Arch = f.Machine.String()
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		buf := make([]byte, p.Filesz)
		if _, err := p.ReadAt(buf, 0); err == nil {
			b.Interp = strings.TrimRight(string(buf), "\x00")
		}
	}
	if strings.Contains(b.Interp, "musl") {
		b.Platform.Libc = Musl
	}
	syms, _ := f.ImportedSymbols()
	for _, s := range syms {
		if v, ok := strings.CutPrefix(s.Version, "GLIBC_"); ok {
			b.Platform.Libc = Glibc
			if ver, ok := ParseVersion(v); ok && b.Platform.LibcVersion.Less(ver) {
				b.Platform.LibcVersion = ver
			}
		}
	}
	b.Libs, _ = f.ImportedLibraries()
	origin := filepath.Dir(path)
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		vals, _ := f.DynString(tag)
		for _, v := range vals {
			for _, d := range strings.Split(v, ":") {
				b.Search = append(b.Search, strings.NewReplacer("$ORIGIN", origin, "${ORIGIN}", origin).Replace(d))
			}
		}
	}
	b.Search = append(b.Search, origin, filepath.Dir(origin))
	return b, nil
}

// Host is a platform that runs binaries: the platform plus its dynamic
// linking environment.
type Host struct {
	Platform Platform
	// Libs are the sonames known to the dynamic loader; nil when unknown
	// (then shared libraries are not checked).
	Libs map[string]bool
	// LibraryPath is LD_LIBRARY_PATH.
	LibraryPath []string
	// Exists reports whether a file exists (os.Stat by default).
	Exists func(string) bool
}

var (
	hostOnce sync.Once
	hostVal  Host
)

// LocalHost is this machine as a Host, detected once.
func LocalHost() Host {
	hostOnce.Do(func() {
		hostVal = Host{Platform: Local(), LibraryPath: filepath.SplitList(os.Getenv("LD_LIBRARY_PATH"))}
		for _, ldconfig := range []string{"ldconfig", "/sbin/ldconfig"} {
			if out, err := exec.Command(ldconfig, "-p").Output(); err == nil {
				hostVal.Libs = ParseLdconfig(string(out))
				break
			}
		}
	})
	return hostVal
}

// ParseLdconfig returns the library names of `ldconfig -p` output.
func ParseLdconfig(out string) map[string]bool {
	libs := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if name, _, ok := strings.Cut(l, " ("); ok && strings.Contains(name, ".so") {
			libs[name] = true
		}
	}
	return libs
}

func (h Host) exists(p string) bool {
	if h.Exists != nil {
		return h.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

// CanRun returns why b cannot run on h, or "" when it can.
func (h Host) CanRun(b Binary) string {
	name := filepath.Base(b.Path)
	hp, bp := h.Platform, b.Platform
	if hp.OS != "" && bp.OS != hp.OS {
		return fmt.Sprintf("%s is built for %s, this machine runs %s", name, bp.OS, hp.OS)
	}
	if hp.Arch != "" && bp.Arch != hp.Arch {
		return fmt.Sprintf("%s is built for %s, this machine is %s", name, bp.Arch, hp.Arch)
	}
	if b.Interp != "" && !h.exists(b.Interp) {
		return fmt.Sprintf("the dynamic loader %s of %s is missing here", b.Interp, name)
	}
	if bp.Libc != "" && b.Interp != "" {
		switch {
		case hp.Libc == "" && bp.Libc == Glibc:
			return fmt.Sprintf("%s needs glibc %s, this machine has none", name, bp.LibcVersion)
		case hp.Libc != "" && hp.Libc != bp.Libc:
			return fmt.Sprintf("%s needs %s, this machine has %s", name, bp.Libc, hp.Libc)
		case bp.Libc == Glibc && !hp.LibcVersion.IsZero() && hp.LibcVersion.Less(bp.LibcVersion):
			return fmt.Sprintf("%s needs glibc %s, this machine has glibc %s", name, bp.LibcVersion, hp.LibcVersion)
		}
	}
	if h.Libs == nil {
		return ""
	}
	for _, l := range b.Libs {
		if h.Libs[l] || h.foundIn(b.Search, l) || h.foundIn(h.LibraryPath, l) {
			continue
		}
		return fmt.Sprintf("the shared library %s of %s is not installed here", l, name)
	}
	return ""
}

func (h Host) foundIn(dirs []string, lib string) bool {
	for _, d := range dirs {
		if d != "" && h.exists(filepath.Join(d, lib)) {
			return true
		}
	}
	return false
}

// CheckRun returns why one of the executables cannot run on this machine,
// or "" when all can.
func CheckRun(paths []string) string {
	h := LocalHost()
	for _, p := range paths {
		b, err := ReadBinary(p)
		if err != nil {
			return fmt.Sprintf("%s is not an executable for this machine: %v", filepath.Base(p), err)
		}
		if why := h.CanRun(b); why != "" {
			return why
		}
	}
	return ""
}
