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

func TestConfigCheckEmbeddedLegacyFile(t *testing.T) {
	p := writeCfg(t, "[fleet]\nowner = \"team\"\n[docker]\ncommand = [\"docker\"]\n")
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("CLOUDSOURCED_FLEET_CONFIG", "")
	out := runConfig(t, "check")
	for _, want := range []string{
		"client configuration ok: " + p,
		`role: client (user "team", orchestrator embedded)`,
		"fleet configuration ok: " + p,
		`role: embedded orchestrator (local daemon), fleet "team", providers local`,
		"fleet.owner is deprecated",
		"[docker] is deprecated",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConfigCheckRemote(t *testing.T) {
	p := writeCfg(t, "[orchestrator]\nurl = \"https://cs.example.com\"\n[fleet]\nmax_machines = 3\n")
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	out := runConfig(t, "check")
	for _, want := range []string{
		"orchestrator remote at https://cs.example.com",
		"[fleet] have no effect in remote mode",
		"fleet configuration: not used by this client",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fleet configuration ok") {
		t.Errorf("remote client validated a fleet configuration:\n%s", out)
	}
}

func TestConfigCheckFleetOnly(t *testing.T) {
	p := writeCfg(t, "user = \"x\"\n[orchestrator]\nmode = \"embedded\"\n[fleet]\nid = \"cp\"\n")
	out := runConfig(t, "check", "--fleet", p)
	if !strings.Contains(out, "role: control plane") || !strings.Contains(out, "ignored by the fleet") || strings.Contains(out, "client configuration") {
		t.Fatalf("unexpected:\n%s", out)
	}
}

func TestConfigShowRedacts(t *testing.T) {
	p := writeCfg(t, `
user = "alice"
[orchestrator]
mode = "embedded"
api_key = "file:key"
[fleet]
owner = "team"
[providers.aws]
backend = "aws-ec2"
region = "eu-central-1"
secret_access_key = "AKIA-literal-secret"
session_token = "env:AWS_SESSION_TOKEN"
`)
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("CLOUDSOURCED_FLEET_CONFIG", "")
	out := runConfig(t, "show")
	if strings.Contains(out, "AKIA-literal-secret") {
		t.Fatalf("secret leaked:\n%s", out)
	}
	for _, want := range []string{"# client configuration: " + p, "# fleet configuration: " + p, "<redacted>", "env:AWS_SESSION_TOKEN", "file:key", "id = 'team'", "region = 'eu-central-1'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "owner =") || strings.Contains(out, "[docker]") {
		t.Errorf("deprecated keys shown:\n%s", out)
	}
}
