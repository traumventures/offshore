package anyfs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mounts returns the mount points of all mounts of file system type
// fstype (e.g. "fuse.offshore-outputs") visible to this process (from
// /proc/self/mountinfo).
func Mounts(fstype string) []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseMountinfo(bufio.NewScanner(f), fstype)
}

func parseMountinfo(sc *bufio.Scanner, fstype string) []string {
	var out []string
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superopts
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || sep+1 >= len(fields) || fields[sep+1] != fstype {
			continue
		}
		out = append(out, unescapeOctal(fields[4]))
	}
	return out
}

// unescapeOctal decodes the \NNN escapes of mountinfo paths.
func unescapeOctal(s string) string {
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

// IsMounted reports whether a mount of type fstype exists at mp.
func IsMounted(fstype, mp string) bool {
	mp = filepath.Clean(mp)
	for _, m := range Mounts(fstype) {
		if m == mp {
			return true
		}
	}
	return false
}

// Covering returns the mount of mounts containing p, or "".
func Covering(mounts []string, p string) string {
	for _, m := range mounts {
		if p == m || strings.HasPrefix(p, m+"/") {
			return m
		}
	}
	return ""
}
