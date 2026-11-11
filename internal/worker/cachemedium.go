package worker

import (
	"log/slog"
	"os"

	"github.com/traumventures/offshore/internal/hostfs"
	"github.com/traumventures/offshore/internal/proto"
)

// checkCacheMedium checks the file system of the cache directory against
// the machine's policy once at startup. Cloud machines require xfs with
// reflink (snapshot capture clones files; the supervisor formats the
// medium so). A worker whose medium breaks the policy keeps serving but
// degrades loudly: an error log, Status.CacheFS with the problems and
// offshore_worker_cache_fs_ok 0. Hosts the user owns (local-docker,
// static) accept any file system with warnings.
func checkCacheMedium(dir string, policy hostfs.Policy, probes hostfs.Probes, log *slog.Logger) *proto.CacheFSStatus {
	st := &proto.CacheFSStatus{Policy: string(policy)}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Error("cache directory unusable", "dir", dir, "err", err)
		st.Problems = []string{err.Error()}
		return st
	}
	info, err := probes.Inspect(dir, true)
	if err != nil {
		st.Problems = []string{err.Error()}
		log.Error("cache file system not inspectable", "dir", dir, "err", err)
		return st
	}
	v := hostfs.Check(info, policy)
	st.Type, st.Reflink, st.OK, st.Problems, st.Warnings = info.Type, info.Reflink, v.OK, v.Problems, v.Warnings
	attrs := []any{"dir", dir, "type", info.Type, "reflink", info.Reflink, "mount", info.Mount.MountPoint, "policy", string(policy)}
	for _, p := range v.Problems {
		log.Error("cache medium breaks the machine policy (degraded: snapshots copy or are skipped)", append(attrs, "problem", p)...)
	}
	for _, w := range v.Warnings {
		log.Warn("cache medium", append(attrs, "warning", w)...)
	}
	if len(v.Problems) == 0 && len(v.Warnings) == 0 {
		log.Info("cache medium", attrs...)
	}
	return st
}
