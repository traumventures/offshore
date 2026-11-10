package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/transport"
)

func freeUDPPort(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}

// setEnv sets the public and secret machine environment the orchestrator
// would produce.
func setEnv(t *testing.T, m auth.Master, id string, expires time.Time, listen string) string {
	t.Helper()
	fp, err := m.MachineFingerprint(id, expires)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDSOURCED_MACHINE_ID", id)
	t.Setenv("CLOUDSOURCED_EXPIRES_AT", strconv.FormatInt(expires.Unix(), 10))
	t.Setenv("CLOUDSOURCED_LISTEN", listen)
	t.Setenv("CLOUDSOURCED_CACHE_DIR", t.TempDir())
	t.Setenv(auth.EnvSignerPub, m.SignerPublic())
	t.Setenv(auth.EnvFingerprint, fp)
	t.Setenv(auth.EnvTLSSeed, auth.EncodeKey(m.MachineSeed(id)))
	return fp
}

func TestConfigFromEnvIdentity(t *testing.T) {
	m, _ := auth.LoadMaster(t.TempDir())
	exp := time.Now().Add(time.Hour)
	fp := setEnv(t, m, "m1", exp, ":0")
	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity.Fingerprint != fp || c.Verifier.Machine != "m1" {
		t.Fatalf("identity %s verifier %+v", c.Identity.Fingerprint, c.Verifier)
	}
	setEnv(t, m, "m1", exp, ":0")
	t.Setenv(auth.EnvFingerprint, "00")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("fingerprint mismatch accepted")
	}
	setEnv(t, m, "m1", exp, ":0")
	t.Setenv(auth.EnvTLSSeed, "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("missing seed accepted")
	}
}

func TestAuthorize(t *testing.T) {
	cases := []struct {
		kind  string
		scope auth.Scope
		ok    bool
	}{
		{proto.KindStatus, auth.ScopeProbe, true},
		{proto.KindStatus, auth.ScopeSession, false},
		{proto.KindStatus, auth.ScopeAdmin, true},
		{proto.KindExec, auth.ScopeSession, true},
		{proto.KindExec, auth.ScopeProbe, false},
		{proto.KindSync, auth.ScopeSession, true},
		{proto.KindSync, auth.ScopeProbe, false},
		{proto.KindFetch, auth.ScopeSession, true},
		{proto.KindFetch, auth.ScopeProbe, false},
		{proto.KindFetch, auth.ScopeAdmin, true},
		{"bogus", auth.ScopeAdmin, false},
	}
	for _, c := range cases {
		err := authorize(c.kind, auth.Claims{Scope: c.scope, Lease: "l1"})
		if (err == nil) != c.ok {
			t.Errorf("%s with %s: %v", c.kind, c.scope, err)
		}
	}
}

// TestServerRejectsTokens runs a server and checks stream authorization
// over QUIC: forged and foreign tokens, wrong scopes and a session token
// used for another lease are rejected.
func TestServerRejectsTokens(t *testing.T) {
	m, _ := auth.LoadMaster(t.TempDir())
	addr := freeUDPPort(t)
	exp := time.Now().Add(time.Hour)
	fp := setEnv(t, m, "m1", exp, addr)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)

	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	var conn *quic.Conn
	for conn == nil {
		if conn, err = transport.Dial(dctx, addr, fp); err != nil {
			if dctx.Err() != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	defer conn.CloseWithError(0, "")
	open := func(token, kind string) *proto.Conn {
		_, pc, err := transport.OpenStream(dctx, conn, token, kind)
		if err != nil {
			t.Fatal(err)
		}
		return pc
	}

	key := m.Signer()
	until := time.Now().Add(time.Hour)
	probe := auth.NewToken(key, auth.ScopeProbe, "m1", "", until)
	session := auth.NewToken(key, auth.ScopeSession, "m1", "l1", until)
	other, _ := auth.LoadMaster(t.TempDir())

	var st proto.Status
	if err := open(probe, proto.KindStatus).Expect(proto.TStatus, &st); err != nil || st.Machine != "m1" {
		t.Fatalf("status with probe token: %+v %v", st, err)
	}
	rejected := map[string][2]string{
		"forged":         {auth.NewToken(other.Signer(), auth.ScopeAdmin, "m1", "", until), proto.KindStatus},
		"other machine":  {auth.NewToken(key, auth.ScopeProbe, "m2", "", until), proto.KindStatus},
		"expired":        {auth.NewToken(key, auth.ScopeProbe, "m1", "", time.Now().Add(-time.Hour)), proto.KindStatus},
		"session status": {session, proto.KindStatus},
		"probe exec":     {probe, proto.KindExec},
		"probe sync":     {probe, proto.KindSync},
		"probe fetch":    {probe, proto.KindFetch},
		"no token":       {"", proto.KindStatus},
		"legacy token":   {"0123456789abcdef0123456789abcdef", proto.KindExec},
	}
	for name, c := range rejected {
		err := open(c[0], c[1]).Expect(proto.TStatus, nil)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Stage != "auth" {
			t.Errorf("%s: want auth error, got %v", name, err)
		}
	}
	// A session token cannot start a session of another lease ...
	pc := open(session, proto.KindExec)
	pc.Send(proto.TExec, proto.Exec{Argv: []string{"true"}, Lease: "l2"})
	var pe *proto.Error
	if err := pc.Expect(proto.TStarted, nil); !errors.As(err, &pe) || pe.Stage != "auth" {
		t.Fatalf("foreign lease: want auth error, got %v", err)
	}
	// ... while its own lease passes authorization (and fails later in
	// preparation because nothing is attached).
	pc = open(session, proto.KindExec)
	pc.Send(proto.TExec, proto.Exec{Argv: []string{"true"}, Lease: "l1", Repo: "/r"})
	if err := pc.Expect(proto.TStarted, nil); !errors.As(err, &pe) || pe.Stage != "prepare" {
		t.Fatalf("own lease: want prepare error, got %v", err)
	}
}
