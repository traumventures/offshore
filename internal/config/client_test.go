package config

import (
	"strings"
	"testing"
)

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// One file may serve the client and the embedded orchestrator: each loader
// reads its own sections.
func TestSingleFileEmbedded(t *testing.T) {
	p := writeFile(t, `
user = "alice"

[orchestrator]
mode = "embedded"

[fleet]
id = "team-a"
max_machines = 2

[providers.local]
backend = "local-docker"
`)
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("CLOUDSOURCED_FLEET_CONFIG", "")
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "alice" || !c.Embedded() || len(c.Warnings) != 0 {
		t.Fatalf("client %+v", c)
	}
	if got := strings.Join(c.FleetSections, ","); got != "[fleet],[providers]" {
		t.Fatalf("fleet sections %q", got)
	}
	if FleetConfigPath() != p {
		t.Fatalf("fleet path %s", FleetConfigPath())
	}
	f, err := LoadFleet(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Fleet.ID != "team-a" || f.Fleet.MaxMachines != 2 || f.ProviderNames()[0] != "local" {
		t.Fatalf("fleet %+v", f.Fleet)
	}
	if !hasWarning(f.Warnings, "[orchestrator], user ignored by the fleet") {
		t.Fatalf("warnings %v", f.Warnings)
	}
}

func TestRemoteClientWarnsAboutFleetSections(t *testing.T) {
	p := writeFile(t, `
[orchestrator]
url = "https://cs.example.com"

[fleet]
max_machines = 9
`)
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.Orchestrator.Mode != "remote" || c.Orchestrator.APIKey != "env:CLOUDSOURCED_API_KEY" {
		t.Fatalf("orchestrator %+v", c.Orchestrator)
	}
	if !hasWarning(c.Warnings, "[fleet] have no effect in remote mode") {
		t.Fatalf("warnings %v", c.Warnings)
	}
	// Without fleet sections there is nothing to warn about.
	t.Setenv("CLOUDSOURCED_CONFIG", writeFile(t, "[orchestrator]\nurl = \"https://cs.example.com\"\n"))
	if c, err := LoadClient(); err != nil || len(c.Warnings) != 0 {
		t.Fatalf("%v %v", c.Warnings, err)
	}
}

func TestSeparateFleetFile(t *testing.T) {
	client := writeFile(t, "[fleet]\nmax_machines = 9\n")
	t.Setenv("CLOUDSOURCED_CONFIG", client)
	t.Setenv("CLOUDSOURCED_FLEET_CONFIG", writeFile(t, "[fleet]\nid = \"f\"\n"))
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(c.Warnings, "have no effect, the fleet configuration is") {
		t.Fatalf("warnings %v", c.Warnings)
	}
	f, err := LoadFleet(FleetConfigPath())
	if err != nil || f.Fleet.ID != "f" || f.Fleet.MaxMachines != 4 {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestDeprecatedOwner(t *testing.T) {
	p := writeFile(t, "[fleet]\nowner = \"legacy\"\n[docker]\ncommand = [\"sudo\", \"-n\", \"docker\"]\n")
	t.Setenv("CLOUDSOURCED_CONFIG", p)
	t.Setenv("USER", "someone")
	f, err := LoadFleet(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Fleet.ID != "legacy" || f.Fleet.Owner != "" {
		t.Fatalf("fleet id %q owner %q", f.Fleet.ID, f.Fleet.Owner)
	}
	for _, w := range []string{"fleet.owner is deprecated", "[docker] is deprecated"} {
		if !hasWarning(f.Warnings, w) {
			t.Errorf("missing warning %q in %v", w, f.Warnings)
		}
	}
	if cmd := f.Providers["local"]["command"].([]any); len(cmd) != 3 || cmd[0] != "sudo" {
		t.Fatalf("legacy docker command %v", cmd)
	}
	// The repository id salt stays the owner, so warm caches survive.
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "legacy" || !hasWarning(c.Warnings, "deprecated fleet.owner") {
		t.Fatalf("user %q warnings %v", c.User, c.Warnings)
	}
	// An explicit user wins; an explicit id leaves the user alone.
	t.Setenv("CLOUDSOURCED_CONFIG", writeFile(t, "user = \"bob\"\n[fleet]\nowner = \"legacy\"\n"))
	if c, _ := LoadClient(); c.User != "bob" {
		t.Fatalf("user %q", c.User)
	}
	t.Setenv("CLOUDSOURCED_CONFIG", writeFile(t, "[fleet]\nid = \"fleet\"\n"))
	if c, _ := LoadClient(); c.User != "someone" {
		t.Fatalf("user %q", c.User)
	}
	if _, err := LoadFleet(writeFile(t, "[fleet]\nid = \"a\"\nowner = \"b\"\n")); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("conflicting id and owner: %v", err)
	}
	if f, err := LoadFleet(writeFile(t, "[fleet]\nid = \"a\"\nowner = \"a\"\n")); err != nil || f.Fleet.ID != "a" {
		t.Fatalf("equal id and owner: %v", err)
	}
	f, _ = LoadFleet(writeFile(t, "[fleet]\nbackend = \"local-docker\"\n"))
	if !hasWarning(f.Warnings, "fleet.backend is deprecated") {
		t.Fatalf("warnings %v", f.Warnings)
	}
}

func TestFleetDefaults(t *testing.T) {
	t.Setenv("USER", "first last")
	f, err := LoadFleet(writeFile(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if f.Fleet.ID != "first-last" || len(f.Warnings) != 0 {
		t.Fatalf("id %q warnings %v", f.Fleet.ID, f.Warnings)
	}
	// An absent file means defaults.
	if f, err := LoadFleet(t.TempDir() + "/missing.toml"); err != nil || f.Fleet.MaxMachines != 4 {
		t.Fatalf("%v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"[fleet]\nid = \"has space\"\n",
		"[fleet]\nunknown_key = 1\n",
		"[nonsense]\nx = 1\n",
	} {
		if _, err := LoadFleet(writeFile(t, bad)); err == nil {
			t.Errorf("fleet accepted:\n%s", bad)
		}
	}
	for _, bad := range []string{
		"[orchestrator]\nmode = \"remote\"\n",
		"[orchestrator]\nmode = \"magic\"\n",
		"[orchestrator]\nurl = \"http://x\"\nfingerprint = \"ab\"\n",
		"[orchestrator]\nurl = \"https://x\"\nclient_cert = \"c.pem\"\n",
		"user = \" \"\n",
		"[fleet]\nunknown_key = 1\n",
	} {
		t.Setenv("CLOUDSOURCED_CONFIG", writeFile(t, bad))
		if _, err := LoadClient(); err == nil {
			t.Errorf("client accepted:\n%s", bad)
		}
	}
	// Fleet validation errors do not break the client (remote mode).
	t.Setenv("CLOUDSOURCED_CONFIG", writeFile(t, "[orchestrator]\nurl = \"https://x\"\n[fleet]\npolicy = \"fifo\"\n"))
	if _, err := LoadClient(); err != nil {
		t.Fatal(err)
	}
}
