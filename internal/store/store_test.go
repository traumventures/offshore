package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestDirGrantScopes(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGranter(context.Background(), Config{Name: "local", Dir: dir, Prefix: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	gr, err := g.Grant(context.Background(), []string{RepoScope("r1"), "/repos/r1/", EnvScope("e")}, time.Hour)
	if err != nil || len(gr.Scopes) != 2 || gr.Store != "local" {
		t.Fatalf("grant %+v %v", gr, err)
	}
	st, err := Open(gr, "repos/r1/chunks", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), "ab/cd", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	// Another view of the same prefix sees the object.
	st2, _ := Open(gr, "repos/r1", 0)
	rc, _, err := st2.Get(context.Background(), "chunks/ab/cd")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "x" {
		t.Fatalf("content %q", b)
	}
	for _, bad := range []string{"repos/r2", "repos/r10", "repos", "../pool/repos/r2", "envs"} {
		if _, err := Open(gr, bad, 0); !errors.Is(err, ErrScope) {
			t.Errorf("scope %q: %v", bad, err)
		}
	}
	if gr.Expired(time.Now(), time.Hour) {
		t.Fatal("dir grants do not expire")
	}
}

type fakeSTS struct{ in *sts.AssumeRoleInput }

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.in = in
	exp := time.Now().Add(time.Hour)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("AK"), SecretAccessKey: aws.String("SK"), SessionToken: aws.String("ST"), Expiration: &exp}}, nil
}

func TestS3GrantAssumesScopedRole(t *testing.T) {
	f := &fakeSTS{}
	g := &s3Granter{c: Config{Name: "aws-ec2", Bucket: "bkt", Prefix: "/cache/pool/", RoleARN: "arn:aws:iam::1:role/r"}, sts: f}
	gr, err := g.Grant(context.Background(), []string{RepoScope("r1")}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if gr.AccessKeyID != "AK" || gr.SessionToken != "ST" || gr.Prefix != "cache/pool" || gr.Expires == 0 {
		t.Fatalf("grant %+v", gr)
	}
	if aws.ToInt32(f.in.DurationSeconds) != 900 {
		t.Fatalf("duration %d (minimum is 15 minutes)", aws.ToInt32(f.in.DurationSeconds))
	}
	var pol struct {
		Statement []struct {
			Resource  any
			Condition map[string]map[string][]string
		}
	}
	if err := json.Unmarshal([]byte(aws.ToString(f.in.Policy)), &pol); err != nil {
		t.Fatal(err)
	}
	if res := pol.Statement[0].Resource.([]any); len(res) != 1 || res[0] != "arn:aws:s3:::bkt/cache/pool/repos/r1/*" {
		t.Fatalf("object resources %v", res)
	}
	if p := pol.Statement[1].Condition["StringLike"]["s3:prefix"]; len(p) != 1 || p[0] != "cache/pool/repos/r1/*" {
		t.Fatalf("list prefixes %v", p)
	}
	// Static credentials without a role.
	g2, err := newS3Granter(context.Background(), Config{Name: "storage", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if gr, _ := g2.Grant(context.Background(), []string{"x"}, time.Hour); gr.AccessKeyID != "a" || gr.SessionToken != "" {
		t.Fatalf("static grant %+v", gr)
	}
	if _, err := newS3Granter(context.Background(), Config{Name: "storage", Bucket: "b"}); err == nil {
		t.Fatal("no credentials and no role accepted")
	}
	if _, err := Open(Grant{Bucket: "b", Scopes: []string{"x"}}, "x", 0); err == nil {
		t.Fatal("grant without credentials opened")
	}
}

// A store of a provider configured with a shared config profile assumes
// its role with that profile's credentials, not the default chain.
func TestS3GranterProfile(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	os.WriteFile(creds, []byte("[ops]\naws_access_key_id = AKIDOPS\naws_secret_access_key = secret\n"), 0o600)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	g, err := newS3Granter(context.Background(), Config{Name: "aws", Bucket: "b", Region: "eu-central-1",
		RoleARN: "arn:aws:iam::1:role/r", Profile: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := g.sts.(*sts.Client).Options().Credentials.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AKIDOPS" {
		t.Fatalf("credentials %+v %v", c, err)
	}
}
