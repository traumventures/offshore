package awsec2

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveBake bakes one real image in the account of the AWS profile
// OFFSHORE_LIVE_BAKE_PROFILE (stack "offshore") from the environment
// image tag OFFSHORE_LIVE_BAKE (for example "<hash>-amd64"). The baked
// image stays (it is the one later machines use); the builder is
// terminated.
func TestLiveBake(t *testing.T) {
	tag := os.Getenv("OFFSHORE_LIVE_BAKE")
	if tag == "" {
		t.Skip("set OFFSHORE_LIVE_BAKE to an environment image tag")
	}
	p, err := open(map[string]any{"profile": os.Getenv("OFFSHORE_LIVE_BAKE_PROFILE"), "region": os.Getenv("AWS_REGION")})
	if err != nil {
		t.Fatal(err)
	}
	e := p.(*EC2)
	ctx := context.Background()
	st, err := e.describeStack(ctx)
	if err != nil || st == nil {
		t.Fatalf("stack: %v", err)
	}
	out := outputs(st)
	base, err := e.amiID(ctx, archAMD64)
	if err != nil {
		t.Fatal(err)
	}
	repo := out["RepositoryUri"]
	j := bakeJob{base: base, image: repo + ":" + tag, arch: archAMD64, region: e.region, registry: repo[:strings.Index(repo, "/")],
		profile: out["InstanceProfileArn"], sg: out["SecurityGroupId"], subnet: e.subnet, env: "live-bake-test"}
	t0 := time.Now()
	ami, err := e.bake.bake(ctx, j)
	if err != nil {
		t.Fatalf("bake: %v (ami %q)", err, ami)
	}
	t.Logf("baked %s from %s in %s", ami, base, time.Since(t0).Round(time.Second))
}
