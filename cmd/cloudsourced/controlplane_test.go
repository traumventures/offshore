package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/fleet"
)

func parseOK(t *testing.T, args ...string) *cpOptions {
	t.Helper()
	t.Setenv("CLOUDSOURCED_HOSTNAME", "")
	t.Setenv("CLOUDSOURCED_LISTEN", "")
	o, err := parseControlplaneFlags(append([]string{"--state-dir", t.TempDir()}, args...))
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return o
}

func TestControlplaneFlagDefaults(t *testing.T) {
	cases := []struct {
		args             []string
		acme             bool
		listen, acmeHTTP string
	}{
		{nil, false, ":7443", ""},
		{[]string{"--hostname", "cs.example.com"}, true, ":443", ":80"},
		{[]string{"--hostname", "cs.example.com."}, true, ":443", ":80"},
		{[]string{"--hostname", "cs.example.com", "--acme=false"}, false, ":7443", ""},
		{[]string{"--hostname", "203.0.113.7"}, false, ":7443", ""},
		{[]string{"--hostname", "cs.example.com", "--listen", ":8443", "--acme-http", "off"}, true, ":8443", ""},
		{[]string{"--hostname", "cs.example.com", "--acme-http", ":8080"}, true, ":443", ":8080"},
		{[]string{"--hostname", "cs.example.com", "--insecure-http"}, false, ":7443", ""},
	}
	for _, c := range cases {
		o := parseOK(t, c.args...)
		if o.ACME != c.acme || o.Listen != c.listen || o.ACMEHTTP != c.acmeHTTP {
			t.Errorf("%v: acme=%v listen=%q acme-http=%q, want %v %q %q", c.args, o.ACME, o.Listen, o.ACMEHTTP, c.acme, c.listen, c.acmeHTTP)
		}
		if o.Hostname != "" && strings.HasSuffix(o.Hostname, ".") {
			t.Errorf("%v: hostname %q keeps the trailing dot", c.args, o.Hostname)
		}
	}
}

func TestControlplaneHostnameFromEnv(t *testing.T) {
	t.Setenv("CLOUDSOURCED_HOSTNAME", "cs.example.com")
	o, err := parseControlplaneFlags([]string{"--state-dir", t.TempDir()})
	if err != nil || !o.ACME || o.Listen != ":443" {
		t.Fatalf("env hostname: %+v %v", o, err)
	}
}

func TestControlplaneFlagErrors(t *testing.T) {
	t.Setenv("CLOUDSOURCED_HOSTNAME", "")
	for _, args := range [][]string{
		{"--acme"},
		{"--acme", "--hostname", "203.0.113.7"},
		{"--acme", "--hostname", "localhost"},
		{"--acme", "--hostname", "cs.example.com", "--insecure-http"},
		{"--tls-cert", "c.pem"},
		{"--tls-cert", "c.pem", "--tls-key", "k.pem", "--insecure-http"},
		{"extra"},
	} {
		if _, err := parseControlplaneFlags(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestControlplaneACMEManager(t *testing.T) {
	o := parseOK(t, "--hostname", "cs.example.com", "--acme-email", "ops@example.com", "--acme-directory", "https://acme.invalid/dir")
	tc, mgr, st, err := cpTLS(o, o.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if mgr == nil || st.TLS != "acme" || st.Fingerprint != "" {
		t.Fatalf("manager %v state %+v", mgr, st)
	}
	if mgr.Email != "ops@example.com" || mgr.Client == nil || mgr.Client.DirectoryURL != "https://acme.invalid/dir" {
		t.Errorf("manager not configured: email %q client %+v", mgr.Email, mgr.Client)
	}
	if dc, ok := mgr.Cache.(autocert.DirCache); !ok || string(dc) != filepath.Join(o.StateDir, "acme") {
		t.Errorf("cache %#v, want DirCache in the state dir", mgr.Cache)
	}
	ctx := context.Background()
	if err := mgr.HostPolicy(ctx, "cs.example.com"); err != nil {
		t.Errorf("host policy rejects the hostname: %v", err)
	}
	if err := mgr.HostPolicy(ctx, "evil.example.com"); err == nil {
		t.Error("host policy accepts other names")
	}
	if !slices.Contains(tc.NextProtos, "acme-tls/1") || tc.GetCertificate == nil || tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLS config does not serve ACME: %+v", tc.NextProtos)
	}
	if got := suggestURLs(o.Hostname, st.TLS, "[::]:443"); !slices.Equal(got, []string{"https://cs.example.com"}) {
		t.Errorf("acme urls %v", got)
	}
}

func leaf(t *testing.T, tc *tls.Config) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(tc.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestControlplaneSelfSigned(t *testing.T) {
	o := parseOK(t)
	tc, mgr, st, err := cpTLS(o, o.StateDir)
	if err != nil || mgr != nil || st.TLS != "self-signed" || len(st.Fingerprint) != 64 {
		t.Fatalf("self-signed: %v %v %+v", err, mgr, st)
	}
	fp := st.Fingerprint
	// The file format is what clients and test/e2e/run.sh read.
	var id struct{ Fingerprint string }
	b, _ := os.ReadFile(filepath.Join(o.StateDir, "tls.json"))
	if json.Unmarshal(b, &id); id.Fingerprint != fp {
		t.Fatalf("tls.json fingerprint %q, want %q", id.Fingerprint, fp)
	}
	// Persistent across restarts.
	_, _, st2, _ := cpTLS(o, o.StateDir)
	if st2.Fingerprint != fp {
		t.Fatal("fingerprint changed on restart")
	}
	_ = tc

	// --hostname with --acme=false adds a SAN (new certificate once).
	o.Hostname, o.ACME = "cs.example.com", false
	tc, _, st3, err := cpTLS(o, o.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Fingerprint == fp {
		t.Fatal("certificate without the hostname was kept")
	}
	if err := leaf(t, tc).VerifyHostname("cs.example.com"); err != nil {
		t.Errorf("SAN missing: %v", err)
	}
	_, _, st4, _ := cpTLS(o, o.StateDir)
	if st4.Fingerprint != st3.Fingerprint {
		t.Error("certificate covering the hostname was replaced")
	}
	// Dropping --hostname keeps the certificate (fingerprint stays pinned).
	o.Hostname = ""
	if _, _, st5, _ := cpTLS(o, o.StateDir); st5.Fingerprint != st3.Fingerprint {
		t.Error("certificate replaced without hostname")
	}
	// IP addresses become IP SANs.
	o.Hostname = "203.0.113.7"
	tc, _, _, _ = cpTLS(o, o.StateDir)
	if err := leaf(t, tc).VerifyHostname("203.0.113.7"); err != nil {
		t.Errorf("IP SAN missing: %v", err)
	}
}

func TestControlplaneCertFile(t *testing.T) {
	id, err := newSelfSigned("cs.example.com", 3600e9)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cert, []byte(id.CertPEM), 0o600)
	os.WriteFile(key, []byte(id.KeyPEM), 0o600)
	o := parseOK(t, "--hostname", "cs.example.com", "--tls-cert", cert, "--tls-key", key)
	if o.ACME {
		t.Fatal("ACME enabled although a certificate was given")
	}
	_, mgr, st, err := cpTLS(o, o.StateDir)
	if err != nil || mgr != nil || st.TLS != "file" {
		t.Fatalf("file cert: %v %v %+v", err, mgr, st)
	}
	if blk, _ := pem.Decode([]byte(id.CertPEM)); blk == nil {
		t.Fatal("bad PEM")
	}
}

func TestSuggestURLs(t *testing.T) {
	if got := suggestURLs("cs.example.com", "self-signed", "10.0.0.5:7443"); !slices.Equal(got, []string{"https://cs.example.com:7443", "https://10.0.0.5:7443"}) {
		t.Errorf("got %v", got)
	}
	if got := suggestURLs("", "off", "127.0.0.1:80"); !slices.Equal(got, []string{"http://127.0.0.1"}) {
		t.Errorf("got %v", got)
	}
	for _, u := range suggestURLs("", "self-signed", "[::]:7443") {
		if !strings.HasPrefix(u, "https://") || !strings.HasSuffix(u, ":7443") {
			t.Errorf("interface url %q", u)
		}
	}
}

func TestClientConfigBlock(t *testing.T) {
	dir := t.TempDir()
	noProbe := func(string) (string, bool, error) { t.Fatal("probed although state exists"); return "", false, nil }
	st := cpState{TLS: "self-signed", Fingerprint: "ab12", URLs: []string{"https://10.0.0.5:7443"}}
	b, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, cpStateFile), b, 0o644)
	got, err := clientConfigBlock(dir, "", "", "env:CLOUDSOURCED_API_KEY", noProbe)
	want := "[orchestrator]\nmode = \"remote\"\nurl = \"https://10.0.0.5:7443\"\nfingerprint = \"ab12\"\napi_key = \"env:CLOUDSOURCED_API_KEY\"\n"
	if err != nil || got != want {
		t.Fatalf("self-signed block:\n%s\nwant:\n%s (%v)", got, want, err)
	}
	// What the client writes must validate as remote mode.
	if !strings.Contains(got, `mode = "remote"`) {
		t.Fatal("no remote mode")
	}

	st = cpState{TLS: "acme", Hostname: "cs.example.com", URLs: []string{"https://cs.example.com"}}
	b, _ = json.Marshal(st)
	os.WriteFile(filepath.Join(dir, cpStateFile), b, 0o644)
	got, _ = clientConfigBlock(dir, "", "", "file:/k", noProbe)
	if strings.Contains(got, "fingerprint") || !strings.Contains(got, `url = "https://cs.example.com"`) {
		t.Errorf("acme block pins a fingerprint or lacks the URL:\n%s", got)
	}

	// No local state: trusted certificates need no pin, others are fetched.
	empty := t.TempDir()
	got, _ = clientConfigBlock(empty, "https://cs.example.com", "", "k", func(string) (string, bool, error) { return "ff", true, nil })
	if strings.Contains(got, "fingerprint") {
		t.Errorf("trusted certificate pinned:\n%s", got)
	}
	got, _ = clientConfigBlock(empty, "https://1.2.3.4:7443", "", "k", func(string) (string, bool, error) { return "ff", false, nil })
	if !strings.Contains(got, `fingerprint = "ff"`) || !strings.Contains(got, "# fetched") {
		t.Errorf("untrusted certificate not pinned:\n%s", got)
	}
	if _, err := clientConfigBlock(empty, "", "", "k", noProbe); err == nil {
		t.Error("no URL accepted")
	}
}

func TestOwnContainerID(t *testing.T) {
	id := strings.Repeat("0123456789abcdef", 4)
	mi := "1203 1180 0:63 / / rw,relatime master:1 - overlay overlay rw\n" +
		"1220 1203 259:2 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/md1 rw\n" +
		"1221 1203 259:2 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/md1 rw\n"
	if got := ownContainerID(strings.NewReader(mi)); got != id {
		t.Fatalf("got %q", got)
	}
	if got := ownContainerID(strings.NewReader("1 0 0:1 / / rw - ext4 /dev/sda rw\n")); got != "" {
		t.Fatalf("host: got %q", got)
	}
}

func TestTranslateMount(t *testing.T) {
	ms := []dockerMount{
		{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		{Source: "/var/lib/docker/volumes/cs-state/_data", Destination: "/var/lib/cloudsourced"},
		{Source: "/srv/cs-bin", Destination: "/var/lib/cloudsourced/bin"},
	}
	for in, want := range map[string]string{
		"/var/lib/cloudsourced/envs/x":           "/var/lib/docker/volumes/cs-state/_data/envs/x",
		"/var/lib/cloudsourced/bin/cloudsourced": "/srv/cs-bin/cloudsourced",
	} {
		if got, ok := translateMount(in, ms); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := translateMount("/usr/local/bin/cloudsourced", ms); ok {
		t.Error("unmounted path translated")
	}
	if _, ok := translateMount("/var/lib/cloudsourcedX/a", ms); ok {
		t.Error("prefix without separator matched")
	}
}

func TestMachineBinary(t *testing.T) {
	u := &config.FleetConfig{Providers: map[string]map[string]any{
		"local": {"backend": "local-docker"},
		"fixed": {"backend": "local-docker", "binary": "/opt/cs"},
		"aws":   {"backend": "aws-ec2"},
	}}
	setMachineBinary(u, "/host/state/bin/cloudsourced")
	if u.Providers["local"]["binary"] != "/host/state/bin/cloudsourced" || u.Providers["fixed"]["binary"] != "/opt/cs" || u.Providers["aws"]["binary"] != nil {
		t.Fatalf("providers %v", u.Providers)
	}
	t.Setenv("CLOUDSOURCED_IN_CONTAINER", "")
	if b, _ := resolveMachineBinary("self", t.TempDir(), t.Logf); b != "" {
		t.Errorf("self: %q", b)
	}
	if b, _ := resolveMachineBinary("/host/cs", t.TempDir(), t.Logf); b != "/host/cs" {
		t.Errorf("explicit: %q", b)
	}
	// In a container the binary is exported to the state directory; without
	// a Docker daemon to ask, its path is used as is (same-path mount).
	t.Setenv("CLOUDSOURCED_IN_CONTAINER", "1")
	t.Setenv("CLOUDSOURCED_DOCKER", "false")
	dir := t.TempDir()
	b, err := resolveMachineBinary("", dir, t.Logf)
	if err != nil || b != filepath.Join(dir, "bin", "cloudsourced") {
		t.Fatalf("container: %q %v", b, err)
	}
	if fi, err := os.Stat(b); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exported binary not executable: %v", err)
	}
}

func TestLoadAPIKeyNeverLogsSource(t *testing.T) {
	t.Setenv("CLOUDSOURCED_API_KEY", "")
	dir := t.TempDir()
	key, src, gen, err := loadAPIKey("", dir)
	if err != nil || !gen || src != filepath.Join(dir, "api-key") || strings.Contains(src, key) {
		t.Fatalf("%q %q %v %v", key, src, gen, err)
	}
	key2, _, gen2, _ := loadAPIKey("", dir)
	if key2 != key || gen2 {
		t.Fatal("key not persistent")
	}
	t.Setenv("CLOUDSOURCED_API_KEY", "cs_0123456789abcdef")
	if _, src, _, _ := loadAPIKey("", dir); src != "$CLOUDSOURCED_API_KEY" {
		t.Fatalf("source %q", src)
	}
}

// TestControlplaneServe starts the control plane (self-signed, hostname as
// SAN), checks /healthz with the pinned fingerprint, info/health, and
// shuts it down gracefully with SIGTERM.
func TestControlplaneServe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOUDSOURCED_CONFIG", filepath.Join(dir, "none.toml"))
	t.Setenv("CLOUDSOURCED_DOCKER", "true") // empty fleet
	t.Setenv("CLOUDSOURCED_API_KEY", "")
	t.Setenv("CLOUDSOURCED_IN_CONTAINER", "")
	o := parseOK(t, "--listen", "127.0.0.1:0", "--hostname", "cs.example.com", "--acme=false")
	o.StateDir = filepath.Join(dir, "state")
	done := make(chan error, 1)
	go func() { done <- serveControlplane(o) }()
	var st *cpState
	for i := 0; i < 200 && st == nil; i++ {
		st, _ = readCPState(o.StateDir)
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil {
		t.Fatal("control plane did not start")
	}
	if st.TLS != "self-signed" || !strings.HasPrefix(st.URLs[0], "https://cs.example.com:") || st.APIKey != filepath.Join(o.StateDir, "api-key") {
		t.Fatalf("state %+v", st)
	}
	r, err := fleet.NewRemote("https://"+st.Listen, st.Fingerprint, "wrong-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong key accepted: %v", err)
	}
	key, _ := os.ReadFile(filepath.Join(o.StateDir, "api-key"))
	r, _ = fleet.NewRemote("https://"+st.Listen, st.Fingerprint, strings.TrimSpace(string(key)))
	if _, err := r.Status(context.Background()); err != nil {
		t.Fatalf("status with the pinned fingerprint: %v", err)
	}
	if err := controlplaneHealth([]string{"--state-dir", o.StateDir}); err != nil {
		t.Fatalf("health: %v", err)
	}
	if fp, trusted, err := probeCertificate("https://" + st.Listen); err != nil || trusted || fp != st.Fingerprint {
		t.Fatalf("probe: %q %v %v", fp, trusted, err)
	}
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}
