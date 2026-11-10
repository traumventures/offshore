package image

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// ECRAPI is the part of the ECR client the registry uses.
type ECRAPI interface {
	DescribeImages(ctx context.Context, in *ecr.DescribeImagesInput, opts ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error)
	GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, opts ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// ECR is an Amazon ECR repository. Has uses the API only (no Docker);
// Push logs the CLI in with an SDK token, tags and pushes.
type ECR struct {
	Client ECRAPI
	// Repository returns the repository URI (account.dkr.ecr.region.amazonaws.com/name),
	// e.g. from the provider's stack; it is called lazily.
	Repository func(ctx context.Context) (string, error)
	CLI        CLI
}

func (*ECR) Local() bool { return false }

// RemoteTag maps an image tag to its reference in repo on platform:
// cloudsourced-env:abc on linux/amd64 becomes repo:abc-amd64. The platform
// is always part of the remote tag, independent of the builder's platform.
func RemoteTag(repo, tag string, p Platform) string {
	_, t, ok := strings.Cut(tag, ":")
	if !ok {
		t = tag
	}
	if p != "" && !strings.HasSuffix(t, "-"+p.Arch()) {
		t += "-" + p.Arch()
	}
	return repo + ":" + t
}

func (r *ECR) Ref(ctx context.Context, tag string, p Platform) (string, error) {
	repo, err := r.Repository(ctx)
	if err != nil {
		return "", err
	}
	return RemoteTag(repo, tag, p), nil
}

// splitRef splits repo-uri:tag into the repository name and tag.
func splitRef(ref string) (name, tag string, err error) {
	i := strings.LastIndex(ref, ":")
	j := strings.Index(ref, "/")
	if i < 0 || j < 0 || i < j {
		return "", "", fmt.Errorf("ecr: invalid image reference %q", ref)
	}
	return ref[j+1 : i], ref[i+1:], nil
}

func (r *ECR) Has(ctx context.Context, ref string) (bool, error) {
	name, tag, err := splitRef(ref)
	if err != nil {
		return false, err
	}
	_, err = r.Client.DescribeImages(ctx, &ecr.DescribeImagesInput{RepositoryName: aws.String(name), ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String(tag)}}})
	if err == nil {
		return true, nil
	}
	var nf *ecrtypes.ImageNotFoundException
	if errors.As(err, &nf) || strings.Contains(err.Error(), "ImageNotFound") {
		return false, nil
	}
	return false, fmt.Errorf("ecr: describe image: %w", err)
}

func (r *ECR) Push(ctx context.Context, localRef string, p Platform, log io.Writer) (string, error) {
	repo, err := r.Repository(ctx)
	if err != nil {
		return "", err
	}
	remote := RemoteTag(repo, localRef, p)
	if err := r.login(ctx, repo); err != nil {
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

func (r *ECR) login(ctx context.Context, repo string) error {
	out, err := r.Client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil || len(out.AuthorizationData) == 0 {
		return fmt.Errorf("ecr: authorization token: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	if err != nil {
		return err
	}
	_, pass, _ := strings.Cut(string(raw), ":")
	registry, _, _ := strings.Cut(repo, "/")
	return r.CLI.Run(ctx, strings.NewReader(pass), nil, "login", "--username", "AWS", "--password-stdin", registry)
}
