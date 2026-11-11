package worker

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/sandbox"
)

// cacheBinds returns the bind mounts of the session's slot and machine
// caches: slot caches from the leased slot's data directory, machine
// caches from the machine cache directory (absolute paths, $VAR and ~
// expanded with the session environment; relative paths are inside the
// repository and kept per repository).
func (s *Worker) cacheBinds(req proto.Exec, data string, env []string) ([]sandbox.Bind, error) {
	var binds []sandbox.Bind
	for _, c := range req.Caches {
		switch c.Scope {
		case config.ScopeSlot:
			for _, p := range c.Paths {
				p = filepath.Clean(p)
				binds = append(binds, sandbox.Bind{Src: filepath.Join(data, p), Dst: filepath.Join(req.Repo, p)})
			}
		case config.ScopeMachine:
			if c.Digest != nil {
				b, err := s.digestBinds(req, c, env)
				if err != nil {
					return nil, fmt.Errorf("cache.%s: %w", c.Name, err)
				}
				binds = append(binds, b...)
				continue
			}
			for _, p := range c.Paths {
				src, dst, ok := s.machineCachePath(req, p, env)
				if ok {
					binds = append(binds, sandbox.Bind{Src: src, Dst: dst})
				}
			}
		}
	}
	return binds, nil
}

// machineCachePath maps a machine cache path to its directory on the
// machine (src) and its location in the session (dst).
func (s *Worker) machineCachePath(req proto.Exec, p string, env []string) (src, dst string, ok bool) {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "$") || strings.HasPrefix(p, "~/") {
		dst = expandPath(p, env)
		if !filepath.IsAbs(dst) || dst == "/" {
			return "", "", false
		}
		return filepath.Join(s.cfg.DepsDir, escapePath(dst)), dst, true
	}
	if req.RepoID == "" || !filepath.IsLocal(p) {
		return "", "", false
	}
	p = filepath.Clean(p)
	return filepath.Join(s.cfg.DepsDir, "repos", req.RepoID, escapePath(p)), filepath.Join(req.Repo, p), true
}

func escapePath(p string) string { return strings.ReplaceAll(strings.Trim(p, "/"), "/", "__") }

// errDigestUnsupported is returned until digest caches are provisioned.
var errDigestUnsupported = errors.New("machine caches with digest are not supported by this worker")

func (s *Worker) digestBinds(req proto.Exec, c proto.Cache, env []string) ([]sandbox.Bind, error) {
	return nil, errDigestUnsupported
}
