package proxy

import (
	"context"
	"strings"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// artifacts makes files of the remote build directories available at
// their local path after a build. Test splitting depends only on this
// interface, so the transport can change (fetch over the session today,
// the target overlay later) without changing what users see.
type artifacts interface {
	// Fetch makes the files at the given repository-relative paths
	// available locally. Paths are literal, not globs.
	Fetch(ctx context.Context, s *session, paths []string) error
}

// fetchArtifacts copies the files over the session's connection from the
// latest build slot of the repository (the one the session just used).
type fetchArtifacts struct{}

func (fetchArtifacts) Fetch(ctx context.Context, s *session, paths []string) error {
	globs := make([]string, len(paths))
	for i, p := range paths {
		globs[i] = escapeGlob(p)
	}
	_, err := FetchOutputs(ctx, s.conn, s.token, s.repoID, s.root, globs, proto.CompressZstd)
	return err
}

// escapeGlob quotes the glob metacharacters of a literal path.
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[]{}\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FetchOutputs copies the output files matching globs (relative to root)
// from the latest build slot of the repository on the machine behind conn
// and returns them. compress ("" or proto.CompressZstd) selects the
// transfer encoding.
func FetchOutputs(ctx context.Context, conn *quic.Conn, token, repoID, root string, globs []string, compress string) ([]manifest.Entry, error) {
	st, pc, err := transport.OpenStream(ctx, conn, token, proto.KindFetch)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := pc.Send(proto.TFetch, proto.Fetch{RepoID: repoID, Repo: root, Paths: globs}); err != nil {
		return nil, err
	}
	var outs proto.Outputs
	if err := pc.Expect(proto.TOutputs, &outs); err != nil {
		return nil, err
	}
	if err := ReceiveFiles(pc, root, outs.Files, compress); err != nil {
		return nil, err
	}
	if err := pc.Write(proto.TDone, nil); err != nil {
		return nil, err
	}
	return outs.Files, nil
}
