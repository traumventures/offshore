package orchestrator

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/provider"
)

// TestMachineSecretsAndTokens checks what a machine receives and what a
// lease carries: no shared token, only the TLS seed is secret, the lease
// token is a session token for that lease, probes use probe tokens.
func TestMachineSecretsAndTokens(t *testing.T) {
	f := newFake()
	o := newTestOrch(t, f, nil)
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.specs[0]
	if len(spec.SecretEnv) != 1 || spec.SecretEnv[auth.EnvTLSSeed] == "" {
		t.Fatalf("SecretEnv = %v, want only %s", keys(spec.SecretEnv), auth.EnvTLSSeed)
	}
	for k, v := range spec.Env {
		if strings.Contains(k, "TOKEN") || strings.Contains(k, "KEY") || strings.Contains(v, "PRIVATE") {
			t.Fatalf("public env carries a secret: %s", k)
		}
	}
	pubB, err := auth.DecodeKey(spec.Env[auth.EnvSignerPub], ed25519.PublicKeySize)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := auth.DecodeKey(spec.SecretEnv[auth.EnvTLSSeed], ed25519.SeedSize)
	id, err := auth.MachineIdentity(seed, spec.ID, spec.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if id.Fingerprint != spec.Finger || spec.Env[auth.EnvFingerprint] != spec.Finger || l.Fingerprint != spec.Finger {
		t.Fatalf("fingerprints differ: derived %s spec %s env %s lease %s", id.Fingerprint, spec.Finger, spec.Env[auth.EnvFingerprint], l.Fingerprint)
	}
	v := auth.Verifier{Pub: ed25519.PublicKey(pubB), Machine: l.MachineID}
	c, err := v.Verify(l.Token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Scope != auth.ScopeSession || c.Lease != l.ID || !c.AllowsLease(l.ID) || c.AllowsLease("other") {
		t.Fatalf("lease token claims %+v", c)
	}
	if c.Exp < spec.ExpiresAt.Unix() {
		t.Fatalf("lease token expires before the machine: %d < %d", c.Exp, spec.ExpiresAt.Unix())
	}
	if len(f.tokens) == 0 {
		t.Fatal("no probe")
	}
	for _, tok := range f.tokens {
		c, err := v.Verify(tok)
		if err != nil || c.Scope != auth.ScopeProbe {
			t.Fatalf("probe token %+v, %v", c, err)
		}
	}
	for _, m := range o.Snapshot() {
		if m.Fingerprint != spec.Finger {
			t.Fatalf("snapshot fingerprint %s", m.Fingerprint)
		}
	}
}

// TestFingerprintSurvivesStateLoss recomputes fingerprints from the master
// key: a new orchestrator over the same state dir (only master.key kept)
// trusts derived fingerprints, not provider labels.
func TestFingerprintSurvivesStateLoss(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, err := New(testConfig("fake"), map[string]provider.Provider{"fake": f}, dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	o.ProbeFunc = f.probe
	l, err := o.Acquire(context.Background(), req("r1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := l.Fingerprint
	// A tampered label must not redirect trust.
	f.mu.Lock()
	f.machines[l.MachineID].Fingerprint = "tampered"
	f.mu.Unlock()
	o2, err := New(testConfig("fake"), map[string]provider.Provider{"fake": f}, dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	o2.ProbeFunc = f.probe
	if err := o2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms := o2.Snapshot()
	if len(ms) != 1 || ms[0].Fingerprint != want {
		t.Fatalf("recomputed fingerprint %+v, want %s", ms, want)
	}
	if o2.master.SignerPublic() != o.master.SignerPublic() {
		t.Fatal("signer changed across restarts")
	}
}

func keys(m map[string]string) []string { return provider.SortedKeys(m) }
