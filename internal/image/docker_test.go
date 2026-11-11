package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// fakeDocker writes a docker CLI stand-in that logs its arguments and
// knows the images listed in the file "images".
func fakeDocker(t *testing.T) (CLI, string) {
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$(dirname "$0")
echo "$*" >> "$dir/calls"
case "$1" in
image) grep -qxF "$5" "$dir/images" 2>/dev/null; exit $? ;;
build) for a in "$@"; do [ "$prev" = "-t" ] && echo "$a" >> "$dir/images"; prev=$a; done ;;
login) cat > "$dir/password" ;;
esac
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return CLI{"sh", path}, dir
}

func calls(t *testing.T, dir string) []string {
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestDockerBuilderPlatformAndBase(t *testing.T) {
	cli, dir := fakeDocker(t)
	b := &DockerBuilder{CLI: cli}
	spec := Spec{Tag: "offshore-env:e1", ContextDir: "/ctx", Dockerfile: "/ctx/Dockerfile", BaseTag: "offshore-base:b1", BaseDir: "/base", BaseFile: "/base/Dockerfile"}
	other := LinuxARM64
	if Native() == LinuxARM64 {
		other = LinuxAMD64
	}
	ref, err := b.Build(context.Background(), spec, other, &bytes.Buffer{})
	if err != nil || ref != "offshore-env:e1-"+other.Arch() {
		t.Fatalf("%q %v", ref, err)
	}
	got := strings.Join(calls(t, dir), "\n")
	for _, want := range []string{
		"build --platform " + string(other) + " -t offshore-base:b1-" + other.Arch() + " -f /base/Dockerfile /base",
		"build --platform " + string(other) + " --build-arg OFFSHORE_BASE=offshore-base:b1-" + other.Arch() + " -t offshore-env:e1-" + other.Arch(),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	// Native builds keep the tags; an existing image is reused.
	if ref, err := b.Build(context.Background(), spec, Native(), &bytes.Buffer{}); err != nil || ref != "offshore-env:e1" {
		t.Fatalf("%q %v", ref, err)
	}
	n := len(calls(t, dir))
	if _, err := b.Build(context.Background(), spec, Native(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, dir); len(c) != n+1 || !strings.HasPrefix(c[n], "image inspect") {
		t.Fatalf("existing image rebuilt: %v", c[n:])
	}
	if strings.Contains(strings.Join(calls(t, dir), "\n"), "--build-arg OFFSHORE_BASE=offshore-base:b1 ") {
		t.Fatal("native build must use the default base argument")
	}
}

type fakeECR struct {
	images map[string]bool
	calls  int
}

func (f *fakeECR) DescribeImages(_ context.Context, in *ecr.DescribeImagesInput, _ ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	f.calls++
	if f.images[aws.ToString(in.RepositoryName)+":"+aws.ToString(in.ImageIds[0].ImageTag)] {
		return &ecr.DescribeImagesOutput{}, nil
	}
	return nil, &ecrtypes.ImageNotFoundException{Message: aws.String("not found")}
}

func (f *fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:secret-pass"))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []ecrtypes.AuthorizationData{{AuthorizationToken: aws.String(tok)}}}, nil
}

func TestECRHasAndPush(t *testing.T) {
	cli, dir := fakeDocker(t)
	const repo = "123.dkr.ecr.eu-central-1.amazonaws.com/cs-env"
	api := &fakeECR{images: map[string]bool{"cs-env:abc-amd64": true}}
	r := &ECR{Client: api, CLI: cli, Repository: func(context.Context) (string, error) { return repo, nil }}
	ctx := context.Background()
	ref, _ := r.Ref(ctx, "offshore-env:abc", LinuxAMD64)
	if ok, err := r.Has(ctx, ref); !ok || err != nil {
		t.Fatalf("has %v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
		t.Fatal("Has must not call docker")
	}
	if ok, err := r.Has(ctx, repo+":def-amd64"); ok || err != nil {
		t.Fatalf("missing image: %v %v", ok, err)
	}
	got, err := r.Push(ctx, "offshore-env:def", LinuxAMD64, &bytes.Buffer{})
	if err != nil || got != repo+":def-amd64" {
		t.Fatalf("%q %v", got, err)
	}
	c := calls(t, dir)
	if len(c) != 3 || c[0] != "login --username AWS --password-stdin 123.dkr.ecr.eu-central-1.amazonaws.com" ||
		c[1] != "tag offshore-env:def "+repo+":def-amd64" || c[2] != "push "+repo+":def-amd64" {
		t.Fatalf("calls %v", c)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "password")); string(b) != "secret-pass" {
		t.Fatalf("password %q", b)
	}
}
