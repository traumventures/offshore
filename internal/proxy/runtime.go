package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/adapter"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/daemon"
	"github.com/traumventures/cloudsourced/internal/events"
	"github.com/traumventures/cloudsourced/internal/proto"
)

// runtime executes the phases of an adapter plan for one proxied command.
type phaseRuntime struct {
	repo    *config.Repo
	profile config.Tool
	tool    string
	args    []string
	cwd     string
	rec     *events.Run
	calls   int
}

func (r *phaseRuntime) Root() string        { return r.repo.Root }
func (r *phaseRuntime) Cwd() string         { return r.cwd }
func (r *phaseRuntime) BuildDirs() []string { return r.repo.SlotPaths() }
func (r *phaseRuntime) Warn(format string, a ...any) {
	warn(format, a...)
}

// overlay reports whether artifacts arrive through the output overlay.
func (r *phaseRuntime) overlay() bool { return r.repo.Outputs.Mode == config.OutputsFuse }

// Remote runs one remote phase. The first phase is recorded as the run;
// later phases (doc tests, a remote rerun) are recorded as runs of their
// own.
func (r *phaseRuntime) Remote(ctx context.Context, argv []string, o adapter.RemoteOptions) (int, error) {
	rec := r.rec
	if r.calls > 0 {
		c := *r.rec
		c.Time, c.Timings, c.Usage, c.Cache, c.Exit, c.Error = time.Now(), events.Timings{}, nil, nil, nil, ""
		rec = &c
	}
	r.calls++
	if o.Why != "" {
		rec.Why = o.Why
	}
	opt := remoteRun{argv: argv, stdout: o.Stdout, stderr: o.Stderr, noTTY: o.NoTTY, stdin: o.Stdin, env: o.Env}
	var collected []string
	if o.Collect != nil {
		opt.after = func(ctx context.Context, s *session, code int) error {
			paths, err := o.Collect(code)
			if err != nil || len(paths) == 0 {
				return err
			}
			collected = paths
			if r.overlay() {
				return nil // available after the release, see below
			}
			if _, err := FetchOutputs(ctx, s.conn, s.token, s.repoID, s.root, artifactGlobs(paths), proto.CompressZstd); err != nil {
				return fmt.Errorf("fetch build artifacts: %w", err)
			}
			return nil
		}
	}
	code, err := runRemote(ctx, r.repo, r.profile, r.tool, r.args, r.cwd, rec, opt)
	if err != nil || len(collected) == 0 || !r.overlay() {
		return code, err
	}
	// The release made the build the overlay's latest generation.
	if _, err := daemon.EnsureOutputs(ctx, r.repo.Root); err != nil {
		return code, fmt.Errorf("output overlay: %w", err)
	}
	for _, p := range collected {
		p = strings.TrimSuffix(p, "/**")
		if _, err := os.Stat(filepath.Join(r.repo.Root, p)); err != nil {
			return code, fmt.Errorf("output overlay: %s not visible: %w", p, err)
		}
	}
	return code, nil
}

// artifactGlobs turns literal repository-relative paths into fetch globs;
// a trailing "/**" (a whole directory) stays a glob.
func artifactGlobs(paths []string) []string {
	globs := make([]string, len(paths))
	for i, p := range paths {
		if dir, ok := strings.CutSuffix(p, "/**"); ok {
			globs[i] = escapeGlob(dir) + "/**"
		} else {
			globs[i] = escapeGlob(p)
		}
	}
	return globs
}
