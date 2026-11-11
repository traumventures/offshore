package access

import (
	"path"
	"strings"
)

// Pattern generalizes a slash separated relative path into the pattern
// its files share across builds: a segment containing a hash or a long
// number becomes "*" (keeping a plain extension and a one or two letter
// prefix such as "s-"), and below such a segment, names derived from it
// (containing one of its words) are generalized too. Examples:
//
//	target/debug/deps/libserde-0123456789abcdef.rlib -> target/debug/deps/*.rlib
//	target/debug/incremental/serde-1x2y3z4w5v6u/s-h0b1e3z4ld-1a2b3c-working/dep-graph.bin
//	    -> target/debug/incremental/*/s-*/dep-graph.bin
//	target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json
//	    -> target/debug/.fingerprint/*/*.json
func Pattern(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	var stems []string // words of generalized ancestors
	for i, s := range segs {
		g, words, ok := generalize(s, stems)
		if ok {
			segs[i] = g
			stems = append(stems, words...)
		}
	}
	return strings.Join(segs, "/")
}

// generalize returns the generalized segment and its non-hash words, or
// ok false when the segment stays.
func generalize(seg string, stems []string) (string, []string, bool) {
	base, ext := splitExt(seg)
	toks := strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	hashed, derived := false, false
	var words []string
	for _, t := range toks {
		if isHash(t) {
			hashed = true
			continue
		}
		if len(t) >= 3 {
			words = append(words, t)
			for _, s := range stems {
				if strings.Contains(strings.ToLower(t), strings.ToLower(s)) {
					derived = true
				}
			}
		}
	}
	if !hashed && !derived {
		return "", nil, false
	}
	prefix := ""
	if len(toks) > 1 && len(toks[0]) <= 2 && !isHash(toks[0]) && strings.HasPrefix(base, toks[0]) {
		prefix = base[:len(toks[0])+1] // "s-"
	}
	return prefix + "*" + ext, words, true
}

// splitExt splits a plain extension (letters and digits, at most 8
// characters, not a hash) off a name; dot files have none.
func splitExt(name string) (base, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return name, ""
	}
	e := name[i+1:]
	if len(e) > 8 || isHash(e) || !isAlnum(e) {
		return name, ""
	}
	return name[:i], name[i:]
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// isHash reports a token that is a hash, a random id or a number: hex
// of at least 8 characters with a digit, at least 4 digits, or at least
// 8 letters and digits with at least two of each.
func isHash(t string) bool {
	var digits, hex, letters int
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digits++
			hex++
		case r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F':
			hex++
			letters++
		case r >= 'g' && r <= 'z' || r >= 'G' && r <= 'Z':
			letters++
		default:
			return false
		}
	}
	n := len(t)
	switch {
	case digits == n:
		return n >= 4
	case hex == n:
		return n >= 8 && digits > 0
	}
	return n >= 8 && digits >= 2 && letters >= 2
}

// Match reports whether a path matches a glob of slash separated
// segments: path.Match per segment, "**" matches any number (also zero)
// of segments.
func Match(glob, p string) bool {
	return matchSegs(strings.Split(glob, "/"), strings.Split(strings.Trim(p, "/"), "/"))
}

func matchSegs(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			rest := g[1:]
			for i := 0; i <= len(p); i++ {
				if matchSegs(rest, p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, err := path.Match(g[0], p[0]); err != nil || !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}
