package image

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// OCIR is a repository of the Oracle Cloud Infrastructure container
// registry. The provider supplies the API parts (no OCI SDK here): Exists
// asks the registry API for a tag, Token returns a docker login token for
// the user BEARER_TOKEN.
type OCIR struct {
	// Repository returns the repository reference
	// (registry/namespace/name); it is called lazily.
	Repository func(ctx context.Context) (string, error)
	// Exists reports whether repository name (without registry and
	// namespace) holds tag.
	Exists func(ctx context.Context, name, tag string) (bool, error)
	// Token returns a registry token for docker login.
	Token func(ctx context.Context, registry string) (string, error)
	CLI   CLI
}

func (*OCIR) Local() bool { return false }

func (r *OCIR) Ref(ctx context.Context, tag string, p Platform) (string, error) {
	repo, err := r.Repository(ctx)
	if err != nil {
		return "", err
	}
	return RemoteTag(repo, tag, p), nil
}

// splitOCIRRef splits registry/namespace/name:tag into name and tag.
func splitOCIRRef(ref string) (name, tag string, err error) {
	i := strings.LastIndex(ref, ":")
	parts := strings.SplitN(ref, "/", 3)
	if i < 0 || len(parts) < 3 || i < len(parts[0])+len(parts[1])+2 {
		return "", "", fmt.Errorf("ocir: invalid image reference %q", ref)
	}
	return ref[len(parts[0])+len(parts[1])+2 : i], ref[i+1:], nil
}

func (r *OCIR) Has(ctx context.Context, ref string) (bool, error) {
	name, tag, err := splitOCIRRef(ref)
	if err != nil {
		return false, err
	}
	return r.Exists(ctx, name, tag)
}

func (r *OCIR) Push(ctx context.Context, localRef string, p Platform, log io.Writer) (string, error) {
	repo, err := r.Repository(ctx)
	if err != nil {
		return "", err
	}
	remote := RemoteTag(repo, localRef, p)
	registry, _, _ := strings.Cut(repo, "/")
	tok, err := r.Token(ctx, registry)
	if err != nil {
		return "", err
	}
	if err := r.CLI.Run(ctx, strings.NewReader(tok), nil, "login", "--username", "BEARER_TOKEN", "--password-stdin", registry); err != nil {
		return "", err
	}
	fmt.Fprintf(log, "pushing %s\n", remote)
	if err := r.CLI.Run(ctx, nil, log, "tag", localRef, remote); err != nil {
		return "", err
	}
	if err := r.CLI.Run(ctx, nil, log, "push", remote); err != nil {
		return "", err
	}
	return remote, nil
}
