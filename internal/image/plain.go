package image

import (
	"context"
	"fmt"
	"io"
)

// Plain is a repository in any registry the Docker CLI is logged in to
// (e.g. a self-hosted registry for static hosts).
type Plain struct {
	Repository string // registry/path/name
	CLI        CLI
}

func (*Plain) Local() bool { return false }

func (r *Plain) Ref(_ context.Context, tag string, p Platform) (string, error) {
	return RemoteTag(r.Repository, tag, p), nil
}

// Has asks the registry for the manifest.
func (r *Plain) Has(ctx context.Context, ref string) (bool, error) {
	return r.CLI.Run(ctx, nil, nil, "manifest", "inspect", ref) == nil, nil
}

func (r *Plain) Push(ctx context.Context, localRef string, p Platform, log io.Writer) (string, error) {
	remote := RemoteTag(r.Repository, localRef, p)
	fmt.Fprintf(log, "pushing %s\n", remote)
	if err := r.CLI.Run(ctx, nil, log, "tag", localRef, remote); err != nil {
		return "", err
	}
	if err := r.CLI.Run(ctx, nil, log, "push", remote); err != nil {
		return "", err
	}
	return remote, nil
}
