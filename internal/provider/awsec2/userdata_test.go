package awsec2

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/hostagent"
	"github.com/traumventures/cloudsourced/internal/provider"
)

func TestUserDataHasNoSecrets(t *testing.T) {
	spec := provider.MachineSpec{
		ID: "abc", Image: "env:1", ExpiresAt: time.Unix(2000, 0), IdleStop: 2 * time.Minute,
		Env:       map[string]string{"CLOUDSOURCED_MACHINE_ID": "abc"},
		SecretEnv: map[string]string{"CLOUDSOURCED_TOKEN": "s3cr3t-token", "CLOUDSOURCED_KEY": "-----BEGIN PRIVATE KEY-----"},
		Services:  []provider.Service{{Name: "db", Image: "postgres"}},
	}
	in := &infra{bucket: "bkt", repo: "123.dkr.ecr.eu-central-1.amazonaws.com/cloudsourced"}
	ud, err := userData(spec, "eu-central-1", "cloudsourced", in, "bin/cs", in.repo+":1")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`echo (\S+) \| base64 -d > /etc/cloudsourced/machine.json`).FindStringSubmatch(ud)
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
	var hs hostagent.Spec
	if err := json.Unmarshal(raw, &hs); err != nil {
		t.Fatal(err)
	}
	if hs.Params[paramSecret] != "/cloudsourced/cloudsourced/machines/abc" || hs.Layout.Image != in.repo+":1" ||
		hs.IdleStop != 120 || hs.ExpiresAt != 2000 || len(hs.Layout.SecretKeys) != 2 || hs.Layout.StatusDir == "" {
		t.Fatalf("spec %+v", hs)
	}
	if !strings.Contains(ud, "systemctl enable --now cloudsourced-host.service") {
		t.Fatal("agent unit not started")
	}
}
