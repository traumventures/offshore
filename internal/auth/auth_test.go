package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/transport"
)

func testMaster(t *testing.T) Master {
	t.Helper()
	m, err := LoadMaster(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoadMasterPersists(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	t.Setenv(EnvMasterKeyFile, "")
	dir := t.TempDir()
	m1, err := LoadMaster(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, MasterKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("master key mode %v", fi.Mode().Perm())
	}
	m2, err := LoadMaster(dir)
	if err != nil || m1 != m2 {
		t.Fatalf("reloaded key differs: %v", err)
	}
	if m1 == (Master{}) {
		t.Fatal("zero master key")
	}
}

func TestLoadMasterImport(t *testing.T) {
	var want Master
	for i := range want {
		want[i] = byte(i)
	}
	t.Setenv(EnvMasterKey, want.String())
	if m, err := LoadMaster(t.TempDir()); err != nil || m != want {
		t.Fatalf("env import: %v", err)
	}
	p := filepath.Join(t.TempDir(), "k")
	os.WriteFile(p, []byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"), 0o600)
	t.Setenv(EnvMasterKey, "")
	t.Setenv(EnvMasterKeyFile, p)
	if m, err := LoadMaster(t.TempDir()); err != nil || m != want {
		t.Fatalf("file import (hex): %v", err)
	}
	if _, err := ParseMaster("c2hvcnQ"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestDerivationDeterministic(t *testing.T) {
	m := testMaster(t)
	exp := time.Unix(1_900_000_000, 0)
	a, err := MachineIdentity(m.MachineSeed("m1"), "m1", exp)
	if err != nil {
		t.Fatal(err)
	}
	// Sub-second parts of the expiry do not matter.
	b, err := MachineIdentity(m.MachineSeed("m1"), "m1", exp.Add(400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if a.CertPEM != b.CertPEM || a.KeyPEM != b.KeyPEM || a.Fingerprint != b.Fingerprint {
		t.Fatal("identity is not deterministic")
	}
	fp, err := m.MachineFingerprint("m1", exp)
	if err != nil || fp != a.Fingerprint {
		t.Fatalf("recomputed fingerprint %s != %s (%v)", fp, a.Fingerprint, err)
	}
	other := []struct {
		name string
		fp   func() (string, error)
	}{
		{"other machine", func() (string, error) { return m.MachineFingerprint("m2", exp) }},
		{"other expiry", func() (string, error) { return m.MachineFingerprint("m1", exp.Add(time.Second)) }},
		{"other master", func() (string, error) { return testMaster(t).MachineFingerprint("m1", exp) }},
	}
	for _, o := range other {
		fp, err := o.fp()
		if err != nil || fp == a.Fingerprint {
			t.Fatalf("%s: same fingerprint (%v)", o.name, err)
		}
	}
	if string(m.MachineSeed("m1")) == string(m.MachineSeed("m2")) {
		t.Fatal("machine seeds collide")
	}
	if m.Signer().Equal(ed25519.NewKeyFromSeed(m.MachineSeed(""))) {
		t.Fatal("signer equals a machine key")
	}
}

// TestEd25519QUIC checks that crypto/tls and quic-go accept the derived
// Ed25519 certificate for TLS 1.3 and that pinning still works.
func TestEd25519QUIC(t *testing.T) {
	m := testMaster(t)
	id, err := MachineIdentity(m.MachineSeed("q1"), "q1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", id)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				s, err := c.AcceptStream(ctx)
				if err == nil {
					s.Write([]byte("ok"))
					s.Close()
				}
			}()
		}
	}()
	conn, err := transport.Dial(ctx, ln.Addr().String(), id.Fingerprint)
	if err != nil {
		t.Fatalf("dial with Ed25519 certificate: %v", err)
	}
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read %q %v", buf, err)
	}
	conn.CloseWithError(0, "")
	wrong, _ := m.MachineFingerprint("q2", time.Now().Add(time.Hour))
	if _, err := transport.Dial(ctx, ln.Addr().String(), wrong); err == nil {
		t.Fatal("pinning accepted a wrong fingerprint")
	}
}

func TestTokens(t *testing.T) {
	m := testMaster(t)
	key := m.Signer()
	pub := key.Public().(ed25519.PublicKey)
	now := time.Unix(1_800_000_000, 0)
	v := Verifier{Pub: pub, Machine: "m1", Now: func() time.Time { return now }}
	sign := func(c Claims) string {
		if c.Iat == 0 {
			c.Iat = now.Unix()
		}
		return Sign(key, c)
	}
	good := sign(Claims{Scope: ScopeSession, Machine: "m1", Lease: "l1", Exp: now.Add(time.Hour).Unix()})
	c, err := v.Verify(good)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Allows(ScopeSession) || c.Allows(ScopeProbe) || c.Allows(ScopeAdmin) || !c.AllowsLease("l1") || c.AllowsLease("l2") || c.AllowsLease("") {
		t.Fatalf("session claims %+v", c)
	}
	admin, err := v.Verify(sign(Claims{Scope: ScopeAdmin, Machine: "m1", Exp: now.Add(time.Hour).Unix()}))
	if err != nil || !admin.Allows(ScopeProbe) || !admin.Allows(ScopeSession) || !admin.AllowsLease("l9") {
		t.Fatalf("admin %+v %v", admin, err)
	}
	probe, err := v.Verify(sign(Claims{Scope: ScopeProbe, Machine: "m1", Exp: now.Add(time.Minute).Unix()}))
	if err != nil || probe.Allows(ScopeSession) || probe.AllowsLease("l1") {
		t.Fatalf("probe %+v %v", probe, err)
	}
	// Within the skew an expired token still passes.
	if _, err := v.Verify(sign(Claims{Scope: ScopeProbe, Machine: "m1", Exp: now.Add(-time.Minute).Unix()})); err != nil {
		t.Fatalf("within skew: %v", err)
	}

	otherKey := testMaster(t).Signer()
	payload, sig, _ := strings.Cut(good, ".")
	_, otherSig, _ := strings.Cut(Sign(otherKey, Claims{Scope: ScopeSession, Machine: "m1", Lease: "l1", Exp: now.Add(time.Hour).Unix(), Iat: now.Unix()}), ".")
	forgedPayload, _, _ := strings.Cut(sign(Claims{Scope: ScopeAdmin, Machine: "m1", Exp: now.Add(time.Hour).Unix()}), ".")
	bad := map[string]string{
		"wrong key":        Sign(otherKey, Claims{Scope: ScopeSession, Machine: "m1", Lease: "l1", Exp: now.Add(time.Hour).Unix(), Iat: now.Unix()}),
		"swapped sig":      payload + "." + otherSig,
		"swapped payload":  forgedPayload + "." + sig,
		"expired":          sign(Claims{Scope: ScopeSession, Machine: "m1", Lease: "l1", Exp: now.Add(-time.Hour).Unix()}),
		"future iat":       sign(Claims{Scope: ScopeSession, Machine: "m1", Lease: "l1", Exp: now.Add(time.Hour).Unix(), Iat: now.Add(time.Hour).Unix()}),
		"wrong machine":    sign(Claims{Scope: ScopeSession, Machine: "m2", Lease: "l1", Exp: now.Add(time.Hour).Unix()}),
		"unknown scope":    sign(Claims{Scope: "root", Machine: "m1", Exp: now.Add(time.Hour).Unix()}),
		"session no lease": sign(Claims{Scope: ScopeSession, Machine: "m1", Exp: now.Add(time.Hour).Unix()}),
		"wrong version":    sign(Claims{V: 2, Scope: ScopeProbe, Machine: "m1", Exp: now.Add(time.Hour).Unix()}),
		"empty":            "",
		"garbage":          "abc.def",
		"no dot":           payload,
		"too long":         strings.Repeat("a", MaxTokenLen+1),
	}
	for name, tok := range bad {
		if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := (Verifier{Machine: "m1"}).Verify(good); err == nil {
		t.Error("verifier without key accepted a token")
	}
}
