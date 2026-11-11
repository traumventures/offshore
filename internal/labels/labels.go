// Package labels describes who runs a session: the worktree and branch it
// comes from, the user and the coding agent driving it, plus free labels.
// The proxy computes them once per run; they travel with the session to the
// daemon, the orchestrator (event log) and the worker (status), so `why`,
// `status`, `top` and `report` can attribute machines, cost and waits.
//
// Labels never carry secrets: only names, paths and identifiers listed
// here are read from the environment.
package labels

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Well-known label keys.
const (
	Worktree = "worktree" // absolute path of the repository root (the worktree)
	Repo     = "repo"     // name of the main repository (shared by its worktrees)
	Branch   = "branch"   // checked out branch ("" when detached)
	User     = "user"     // client user (config "user")
	Host     = "host"     // client host name
	Agent    = "agent"    // coding agent, e.g. "codex", "claude-code"; "" for humans
	// AgentSession identifies the agent's conversation/thread when the
	// agent exports one (e.g. CODEX_THREAD_ID).
	AgentSession = "agent_session"
	// Purpose is "prewarm" for runs started by `offshore prewarm`.
	Purpose = "purpose"
)

// PurposePrewarm marks runs of `offshore prewarm`.
const PurposePrewarm = "prewarm"

// Limits keep labels small on the wire and in the event log.
const (
	MaxLabels   = 16
	MaxKeyLen   = 32
	MaxValueLen = 200
)

// EnvAgent overrides agent detection; EnvLabels adds free labels
// ("k=v,k=v").
const (
	EnvAgent  = "OFFSHORE_AGENT"
	EnvLabels = "OFFSHORE_LABELS"
)

// agentProbe detects one agent from the presence of an environment
// variable. Session names a variable holding a non-secret thread id.
type agentProbe struct {
	Var, Agent, Session string
}

// Probes are checked in order; the first present variable wins.
var Probes = []agentProbe{
	{Var: "CODEX_THREAD_ID", Agent: "codex", Session: "CODEX_THREAD_ID"},
	{Var: "CODEX_SANDBOX", Agent: "codex"},
	{Var: "CODEX_MANAGED_BY_NPM", Agent: "codex"},
	{Var: "CLAUDECODE", Agent: "claude-code"},
	{Var: "CLAUDE_CODE_ENTRYPOINT", Agent: "claude-code"},
	{Var: "CURSOR_AGENT", Agent: "cursor"},
	{Var: "GEMINI_CLI", Agent: "gemini-cli"},
	{Var: "OPENCODE", Agent: "opencode"},
	{Var: "AIDER_MODEL", Agent: "aider"},
	{Var: "AMP_THREAD_ID", Agent: "amp", Session: "AMP_THREAD_ID"},
}

// Detect returns the labels of a run in the repository at root (the
// directory holding .offshore.toml). getenv is os.Getenv in production.
func Detect(root, user string, getenv func(string) string) map[string]string {
	l := map[string]string{}
	if root != "" {
		l[Worktree] = root
		if gi, ok := gitInfo(root); ok {
			l[Branch] = gi.branch
			if gi.repo != "" {
				l[Repo] = gi.repo
			}
		}
		if l[Repo] == "" {
			l[Repo] = filepath.Base(root)
		}
	}
	if user != "" {
		l[User] = user
	}
	if h, err := hostname(); err == nil && h != "" {
		l[Host] = h
	}
	if a := getenv(EnvAgent); a != "" {
		l[Agent] = a
	} else {
		for _, p := range Probes {
			if getenv(p.Var) == "" {
				continue
			}
			l[Agent] = p.Agent
			if p.Session != "" {
				l[AgentSession] = getenv(p.Session)
			}
			break
		}
	}
	for k, v := range Parse(getenv(EnvLabels)) {
		l[k] = v
	}
	return Clean(l)
}

// hostname is os.Hostname (replaced in tests).
var hostname = os.Hostname

// UserHost renders the client of a session as user@host (either part may
// be missing; "" without both).
func UserHost(l map[string]string) string {
	u, h := l[User], l[Host]
	switch {
	case u != "" && h != "":
		return u + "@" + h
	case u != "":
		return u
	}
	return h
}

// Parse reads "k=v,k=v" (whitespace around keys and values is trimmed;
// entries without "=" are ignored).
func Parse(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// validKey allows lower-case letters, digits, '_', '-' and '.'.
func validKey(k string) bool {
	if k == "" || len(k) > MaxKeyLen {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Clean drops invalid keys and empty values, truncates long values,
// removes control characters and keeps at most MaxLabels labels (well-known
// keys first, then alphabetically). It returns nil for no labels. Servers
// apply it to labels received from clients.
func Clean(in map[string]string) map[string]string {
	keys := make([]string, 0, len(in))
	for k, v := range in {
		if validKey(k) && strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	if len(keys) > MaxLabels {
		keys = keys[:MaxLabels]
	}
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v := strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, in[k])
		if len(v) > MaxValueLen {
			v = v[:MaxValueLen]
		}
		out[k] = v
	}
	return out
}

// Format renders labels as "k=v k=v" in Clean's order.
func Format(l map[string]string) string {
	c := Clean(l)
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + c[k]
	}
	return strings.Join(parts, " ")
}

// lessKey orders well-known keys first, then alphabetically.
func lessKey(a, b string) bool {
	rank := func(k string) int {
		for i, w := range []string{Worktree, Repo, Branch, User, Host, Agent, AgentSession, Purpose} {
			if k == w {
				return i
			}
		}
		return 100
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	return a < b
}

// Merge returns a copy of base with extra applied on top.
func Merge(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

type gitInfoResult struct {
	branch string
	repo   string // name of the main repository
}

// gitInfo reads the branch and the main repository of the git checkout
// containing root without running git.
func gitInfo(root string) (gitInfoResult, bool) {
	g, ok := findGitDirs(root)
	if !ok {
		return gitInfoResult{}, false
	}
	var r gitInfoResult
	if b, err := os.ReadFile(filepath.Join(g.dir, "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: "); ok {
			r.branch = strings.TrimPrefix(ref, "refs/heads/")
		}
	}
	if filepath.Base(g.common) == ".git" {
		r.repo = filepath.Base(filepath.Dir(g.common))
	} else {
		r.repo = strings.TrimSuffix(filepath.Base(g.common), ".git")
	}
	return r, true
}

// findGitDirs looks for .git in root and its parents.
func findGitDirs(root string) (gitDirs, bool) {
	for dir := root; ; {
		if g, ok := gitDirOf(dir); ok {
			return g, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitDirs{}, false
		}
		dir = parent
	}
}

// HeadCommit returns the commit HEAD points to in the checkout at root
// ("" if unknown). Used to decide whether a prewarm is still fresh.
func HeadCommit(root string) string {
	gi, ok := findGitDirs(root)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gi.dir, "HEAD"))
	if err != nil {
		return ""
	}
	h := strings.TrimSpace(string(b))
	ref, isRef := strings.CutPrefix(h, "ref: ")
	if !isRef {
		return h
	}
	for _, d := range []string{gi.dir, gi.common} {
		if b, err := os.ReadFile(filepath.Join(d, filepath.FromSlash(ref))); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if b, err := os.ReadFile(filepath.Join(gi.common, "packed-refs")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
				return f[0]
			}
		}
	}
	return ""
}

// gitDirs are the git directory of a checkout and the common directory of
// its main repository (equal for the main checkout).
type gitDirs struct{ dir, common string }

// gitDirOf resolves root/.git: a directory (main checkout) or a file
// "gitdir: <path>" (linked worktree, whose commondir names the main .git).
func gitDirOf(root string) (gitDirs, bool) {
	p := filepath.Join(root, ".git")
	fi, err := os.Stat(p)
	if err != nil {
		return gitDirs{}, false
	}
	g := p
	if !fi.IsDir() {
		b, err := os.ReadFile(p)
		if err != nil {
			return gitDirs{}, false
		}
		g = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(g) {
			g = filepath.Join(root, g)
		}
	}
	d := gitDirs{dir: g, common: g}
	if b, err := os.ReadFile(filepath.Join(g, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(g, c)
		}
		d.common = filepath.Clean(c)
	}
	return d, true
}
