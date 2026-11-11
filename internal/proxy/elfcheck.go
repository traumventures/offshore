package proxy

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// The test binaries come from the machine's environment image. Before they
// run here, checkLocalExec makes sure this machine can execute them: same
// architecture, the dynamic loader exists, the C library is at least as new
// as the one they were linked against, and their shared libraries are found.

var goarchMachine = map[string]elf.Machine{
	"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "386": elf.EM_386,
	"arm": elf.EM_ARM, "riscv64": elf.EM_RISCV, "ppc64le": elf.EM_PPC64, "s390x": elf.EM_S390,
}

// checkLocalExec returns why the binaries cannot run on this machine, or
// "" when they can.
func checkLocalExec(paths []string) string {
	env := localLinkEnv()
	for _, p := range paths {
		if why := checkELF(p, env); why != "" {
			return why
		}
	}
	return ""
}

// linkEnv describes the local dynamic linking environment.
type linkEnv struct {
	machine elf.Machine
	glibc   [2]int          // major, minor; zero when unknown or not glibc
	libs    map[string]bool // sonames known to the dynamic loader; nil when unknown
}

var (
	linkEnvOnce sync.Once
	linkEnvVal  linkEnv
)

func localLinkEnv() linkEnv {
	linkEnvOnce.Do(func() {
		linkEnvVal.machine = goarchMachine[runtime.GOARCH]
		if out, err := exec.Command("getconf", "GNU_LIBC_VERSION").Output(); err == nil {
			// "glibc 2.39"
			if f := strings.Fields(string(out)); len(f) == 2 {
				linkEnvVal.glibc, _ = parseVersion(f[1])
			}
		}
		if out, err := exec.Command("ldconfig", "-p").Output(); err == nil {
			linkEnvVal.libs = parseLdconfig(string(out))
		} else if out, err := exec.Command("/sbin/ldconfig", "-p").Output(); err == nil {
			linkEnvVal.libs = parseLdconfig(string(out))
		}
	})
	return linkEnvVal
}

// parseLdconfig returns the library names of `ldconfig -p` output.
func parseLdconfig(out string) map[string]bool {
	libs := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if name, _, ok := strings.Cut(l, " ("); ok && strings.Contains(name, ".so") {
			libs[name] = true
		}
	}
	return libs
}

// parseVersion parses "2.39" into {2, 39}.
func parseVersion(s string) ([2]int, bool) {
	a, b, ok := strings.Cut(s, ".")
	if !ok {
		return [2]int{}, false
	}
	b, _, _ = strings.Cut(b, ".")
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	return [2]int{x, y}, err1 == nil && err2 == nil
}

func versionLess(a, b [2]int) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

func checkELF(path string, env linkEnv) string {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Sprintf("%s is not an executable for this machine: %v", filepath.Base(path), err)
	}
	defer f.Close()
	if env.machine != 0 && f.Machine != env.machine {
		return fmt.Sprintf("the test binaries are built for %s, this machine is %s", f.Machine, env.machine)
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err == nil {
			interp := strings.TrimRight(string(b), "\x00")
			if _, err := os.Stat(interp); err != nil {
				return fmt.Sprintf("the dynamic loader %s of the test binaries is missing here", interp)
			}
		}
	}
	syms, _ := f.ImportedSymbols()
	var need [2]int
	needName := ""
	for _, s := range syms {
		if v, ok := strings.CutPrefix(s.Version, "GLIBC_"); ok {
			if ver, ok := parseVersion(v); ok && versionLess(need, ver) {
				need, needName = ver, s.Version
			}
		}
	}
	if needName != "" {
		if env.glibc == [2]int{} {
			return fmt.Sprintf("the test binaries need glibc (%s), this machine has none", needName)
		}
		if versionLess(env.glibc, need) {
			return fmt.Sprintf("the test binaries need %s, this machine has glibc %d.%d", needName, env.glibc[0], env.glibc[1])
		}
	}
	if env.libs == nil {
		return ""
	}
	libs, _ := f.ImportedLibraries()
	search := searchDirs(f, path)
	for _, l := range libs {
		if env.libs[l] || foundIn(search, l) {
			continue
		}
		return fmt.Sprintf("the shared library %s of the test binaries is not installed here", l)
	}
	return ""
}

// searchDirs are the directories besides the loader cache where a binary's
// libraries are looked up: RPATH/RUNPATH, LD_LIBRARY_PATH and the
// directories cargo adds for tests.
func searchDirs(f *elf.File, path string) []string {
	origin := filepath.Dir(path)
	var dirs []string
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		vals, _ := f.DynString(tag)
		for _, v := range vals {
			for _, d := range strings.Split(v, ":") {
				dirs = append(dirs, strings.NewReplacer("$ORIGIN", origin, "${ORIGIN}", origin).Replace(d))
			}
		}
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("LD_LIBRARY_PATH"))...)
	return append(dirs, origin, filepath.Dir(origin))
}

func foundIn(dirs []string, lib string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, lib)); err == nil {
			return true
		}
	}
	return false
}
