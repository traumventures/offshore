package image

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeBuilder struct {
	id     string
	mu     sync.Mutex
	builds []string // tag@platform
	fail   error
}

func (b *fakeBuilder) ID() string { return b.id }
func (b *fakeBuilder) Build(_ context.Context, s Spec, p Platform, log io.Writer) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return "", b.fail
	}
	if _, err := os.Stat(s.ContextDir); err != nil {
		return "", err
	}
	b.builds = append(b.builds, s.Tag+"@"+string(p))
	io.WriteString(log, "built\n")
	return LocalRef(s.Tag, p), nil
}

type fakeRegistry struct {
	name   string
	local  bool
	mu     sync.Mutex
	images map[string]bool
	pushes []string
	hasErr error
}

func (r *fakeRegistry) Local() bool { return r.local }
func (r *fakeRegistry) Ref(_ context.Context, tag string, p Platform) (string, error) {
	if r.local {
		return LocalRef(tag, p), nil
	}
	return RemoteTag(r.name+"/repo", tag, p), nil
}
func (r *fakeRegistry) Has(_ context.Context, ref string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.images[ref], r.hasErr
}
func (r *fakeRegistry) Push(ctx context.Context, local string, p Platform, _ io.Writer) (string, error) {
	ref := local
	if !r.local {
		ref = RemoteTag(r.name+"/repo", local, p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.images == nil {
		r.images = map[string]bool{}
	}
	r.images[ref] = true
	r.pushes = append(r.pushes, ref)
	return ref, nil
}

func source(t *testing.T, hash string) (Source, *atomic.Int32) {
	n := &atomic.Int32{}
	return Source{Hash: hash, Tag: "cloudsourced-env:" + hash, Context: func() (Spec, string, error) {
		n.Add(1)
		dir, err := os.MkdirTemp(t.TempDir(), "ctx")
		return Spec{Tag: "cloudsourced-env:" + hash, ContextDir: dir}, dir, err
	}}, n
}

func TestPipelineBuildsOncePerHashAndPlatform(t *testing.T) {
	ctx := context.Background()
	var p Pipeline
	b := &fakeBuilder{id: "docker"}
	ecr := &fakeRegistry{name: "ecr"}
	other := &fakeRegistry{name: "ecr2"}
	src, contexts := source(t, "aaa")
	amd := Target{Name: "aws", Platform: LinuxAMD64, Registry: ecr, Builder: b}
	amd2 := Target{Name: "aws2", Platform: LinuxAMD64, Registry: other, Builder: b}
	arm := Target{Name: "aws-arm", Platform: LinuxARM64, Registry: ecr, Builder: b}
	var wg sync.WaitGroup
	refs := make([]string, 6)
	for i, tg := range []Target{amd, amd2, arm, amd, amd2, arm} {
		wg.Add(1)
		go func(i int, tg Target) {
			defer wg.Done()
			ref, err := p.Ensure(ctx, src, tg, io.Discard)
			if err != nil {
				t.Error(err)
			}
			refs[i] = ref
		}(i, tg)
	}
	wg.Wait()
	if len(b.builds) != 2 || contexts.Load() != 2 {
		t.Fatalf("builds %v contexts %d, want one per platform", b.builds, contexts.Load())
	}
	if refs[0] != "ecr/repo:aaa-amd64" || refs[1] != "ecr2/repo:aaa-amd64" || refs[2] != "ecr/repo:aaa-arm64" || refs[3] != refs[0] {
		t.Fatalf("refs %v", refs)
	}
	if len(ecr.pushes) != 2 || len(other.pushes) != 1 {
		t.Fatalf("pushes %v %v", ecr.pushes, other.pushes)
	}
	if ref, ok := p.Ready(amd, "aaa"); !ok || ref != refs[0] {
		t.Fatalf("ready %q %v", ref, ok)
	}
}

func TestPipelineSkipsBuildWhenRegistryHasImage(t *testing.T) {
	var p Pipeline
	b := &fakeBuilder{id: "docker", fail: errors.New("docker must not be called")}
	ecr := &fakeRegistry{name: "ecr", images: map[string]bool{"ecr/repo:bbb-amd64": true}}
	src, contexts := source(t, "bbb")
	ref, err := p.Ensure(context.Background(), src, Target{Name: "aws", Platform: LinuxAMD64, Registry: ecr, Builder: b}, io.Discard)
	if err != nil || ref != "ecr/repo:bbb-amd64" {
		t.Fatalf("%q %v", ref, err)
	}
	if len(ecr.pushes) != 0 || contexts.Load() != 0 {
		t.Fatalf("pushed %v, contexts %d", ecr.pushes, contexts.Load())
	}
	// Without any builder the registry check is enough too.
	ref, err = p.Ensure(context.Background(), src, Target{Name: "aws2", Platform: LinuxAMD64, Registry: ecr}, io.Discard)
	if err != nil || ref != "ecr/repo:bbb-amd64" {
		t.Fatalf("%q %v", ref, err)
	}
}

func TestPipelineLocalRegistryNeedsNoPush(t *testing.T) {
	var p Pipeline
	b := &fakeBuilder{id: "docker"}
	local := &fakeRegistry{name: "local", local: true}
	src, _ := source(t, "ccc")
	ref, err := p.Ensure(context.Background(), src, Target{Name: "local", Platform: Native(), Registry: local, Builder: b}, io.Discard)
	if err != nil || ref != "cloudsourced-env:ccc" {
		t.Fatalf("%q %v", ref, err)
	}
	if len(b.builds) != 1 {
		t.Fatalf("builds %v", b.builds)
	}
}

func TestPipelineFailureBackoff(t *testing.T) {
	var p Pipeline
	b := &fakeBuilder{id: "docker", fail: errors.New("boom")}
	ecr := &fakeRegistry{name: "ecr"}
	src, contexts := source(t, "ddd")
	tg := Target{Name: "aws", Platform: LinuxAMD64, Registry: ecr, Builder: b}
	for i := 0; i < 3; i++ {
		if _, err := p.Ensure(context.Background(), src, tg, io.Discard); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("want build error, got %v", err)
		}
	}
	if contexts.Load() != 1 {
		t.Fatalf("failed build retried %d times within the backoff", contexts.Load())
	}
	old := FailureBackoff
	FailureBackoff = 0
	defer func() { FailureBackoff = old }()
	b.fail = nil
	if _, err := p.Ensure(context.Background(), src, tg, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRefs(t *testing.T) {
	if got := RemoteTag("123.dkr.ecr.eu-central-1.amazonaws.com/cs-env", "cloudsourced-env:abc", LinuxAMD64); got != "123.dkr.ecr.eu-central-1.amazonaws.com/cs-env:abc-amd64" {
		t.Fatal(got)
	}
	if got := RemoteTag("r/x", "cloudsourced-env:abc-arm64", LinuxARM64); got != "r/x:abc-arm64" {
		t.Fatal(got)
	}
	other := LinuxARM64
	if Native() == LinuxARM64 {
		other = LinuxAMD64
	}
	if LocalRef("t:1", Native()) != "t:1" || LocalRef("t:1", other) != "t:1-"+other.Arch() {
		t.Fatal(LocalRef("t:1", other))
	}
	name, tag, err := splitRef("123.dkr.ecr.eu-central-1.amazonaws.com/ns/cs-env:abc-amd64")
	if err != nil || name != "ns/cs-env" || tag != "abc-amd64" {
		t.Fatal(name, tag, err)
	}
}
