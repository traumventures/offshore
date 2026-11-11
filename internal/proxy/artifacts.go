package proxy

import (
	"context"
	"strings"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/offshore/internal/manifest"
	"github.com/traumventures/offshore/internal/proto"
	"github.com/traumventures/offshore/internal/transport"
)

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
