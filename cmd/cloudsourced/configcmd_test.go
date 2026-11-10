package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runConfig(t *testing.T, args ...string) string {
	t.Helper()
	t.Chdir(t.TempDir()) // no repository
	var out bytes.Buffer
	if err := configCmd(args, &out); err != nil {
		t.Fatalf("config %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestConfigCheckEmbeddedSingleFile(t *testing.T) {
	p := writeCfg(t, "user = \"team\"\n[pool]\nid = \"team\"\n[providers.local]\ntype = \"local-docker\"\ncommand = [\"docker\"]\n")
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("CLOUDSOURCED_POOL_CONFIG", "")
	out := runConfig(t, "check")
	for _, want := range []string{
		"client configuration ok: " + p,
		`role: client (user "team", orchestrator embedded)`,
		"pool configuration ok: " + p,
		`role: embedded orchestrator (local daemon), pool "team", providers local`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConfigCheckRemote(t *testing.T) {
	p := writeCfg(t, "[control_plane]\nurl = \"https://cs.example.com\"\n[pool]\nmax_machines = 3\n")
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	out := runConfig(t, "check")
	for _, want := range []string{
		"orchestrator remote at https://cs.example.com",
		"[pool] have no effect in remote mode",
		"pool configuration: not used by this client",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pool configuration ok") {
		t.Errorf("remote client validated a pool configuration:\n%s", out)
	}
}

func TestConfigCheckPoolOnly(t *testing.T) {
	p := writeCfg(t, "user = \"x\"\n[pool]\nid = \"cp\"\n")
	out := runConfig(t, "check", "--pool", p)
	if !strings.Contains(out, "role: control plane") || !strings.Contains(out, "ignored by the pool") || strings.Contains(out, "client configuration") {
		t.Fatalf("unexpected:\n%s", out)
	}
}

func TestConfigShowRedacts(t *testing.T) {
	p := writeCfg(t, `
user = "alice"
[pool]
id = "team"
[providers.aws]
type = "aws-ec2"
region = "eu-central-1"
secret_access_key = "AKIA-literal-secret"
session_token = "env:AWS_SESSION_TOKEN"
`)
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("CLOUDSOURCED_POOL_CONFIG", "")
	out := runConfig(t, "show")
	if strings.Contains(out, "AKIA-literal-secret") {
		t.Fatalf("secret leaked:\n%s", out)
	}
	for _, want := range []string{"# client configuration: " + p, "# pool configuration: " + p, "<redacted>", "env:AWS_SESSION_TOKEN", "id = 'team'", "region = 'eu-central-1'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
