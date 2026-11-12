package awsec2

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/supervisor"
)

func TestUserDataHasNoSecrets(t *testing.T) {
	spec := provider.MachineSpec{
		ID: "abc", Image: "env:1", ExpiresAt: time.Unix(2000, 0), IdleStop: 2 * time.Minute,
		Env:       map[string]string{"OFFSHORE_MACHINE_ID": "abc"},
		SecretEnv: map[string]string{"OFFSHORE_TOKEN": "s3cr3t-token", "OFFSHORE_KEY": "-----BEGIN PRIVATE KEY-----"},
		Services:  []provider.Service{{Name: "db", Image: "postgres"}},
	}
	in := &infra{bucket: "bkt", repo: "123.dkr.ecr.eu-central-1.amazonaws.com/offshore"}
	ud, err := userData(spec, "eu-central-1", "offshore", in, binarySource{Key: "bin/cs"}, in.repo+":1")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`echo (\S+) \| base64 -d > /etc/offshore/machine.json`).FindStringSubmatch(ud)
	if m == nil {
		t.Fatalf("no spec in user data:\n%s", ud)
	}
	raw, _ := base64.StdEncoding.DecodeString(m[1])
	for _, s := range []string{ud, string(raw)} {
		for _, v := range spec.SecretEnv {
			if strings.Contains(s, v) {
				t.Fatalf("secret %q in user data", v)
			}
		}
	}
	var hs supervisor.Spec
	if err := json.Unmarshal(raw, &hs); err != nil {
		t.Fatal(err)
	}
	if hs.Params[paramSecret] != "/offshore/offshore/machines/abc" || hs.Layout.Image != in.repo+":1" ||
		hs.IdleStop != 120 || hs.ExpiresAt != 2000 || len(hs.Layout.SecretKeys) != 2 || hs.Layout.StatusDir == "" {
		t.Fatalf("spec %+v", hs)
	}
	if !strings.Contains(ud, "systemctl enable --now offshore-supervisor.service") {
		t.Fatal("agent unit not started")
	}
	for _, want := range []string{"command -v docker >/dev/null || dnf install", "echo user_data $(date +%s%3N) >> /run/offshore/boot-phases",
		"echo docker $(date +%s%3N)", "aws s3 cp", "wait $binpid"} {
		if !strings.Contains(ud, want) {
			t.Fatalf("user data lacks %q:\n%s", want, ud)
		}
	}
	if hs.Params[paramBucket] != "bkt" {
		t.Fatalf("spec lacks the bucket (binary updates): %+v", hs.Params)
	}
	// A presigned URL replaces the AWS CLI download.
	ud, err = userData(spec, "eu-central-1", "offshore", in, binarySource{Key: "bin/cs", URL: "https://bkt.s3/bin/cs?X-Amz-Signature=x"}, in.repo+":1")
	if err != nil || !strings.Contains(ud, "curl -fsS --retry 5 --retry-all-errors -o /opt/offshore/bin/offshore.tmp 'https://bkt.s3/bin/cs?X-Amz-Signature=x'") ||
		strings.Contains(ud, "aws s3 cp") {
		t.Fatalf("presigned download:\n%s", ud)
	}
}
