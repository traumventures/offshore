package pool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/traumventures/cloudsourced/internal/config"
)

func testCert(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ca bool) (*x509.Certificate, *ecdsa.PrivateKey, tls.Certificate) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: ca, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestControlPlaneMutualTLS checks the optional client certificate
// requirement: the API key alone is not enough, a certificate from another
// CA is rejected, the right certificate plus key works.
func TestControlPlaneMutualTLS(t *testing.T) {
	u := &config.PoolConfig{Pool: config.Pool{ID: "t", MaxLifetime: "24h", IdleStop: "1m", QueueTimeout: "1s", MaxMachines: 1,
		Scheduler: config.Scheduler{Estimator: "static", CPUOvercommit: 1, MemoryOvercommit: 1}},
		Providers: map[string]map[string]any{"img": {"type": "test-img"}}}
	state := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := NewLocal(u, state, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(local, "secret-key-0123456789", state, log)
	if err != nil {
		t.Fatal(err)
	}
	ca, caKey, _ := testCert(t, "ca", nil, nil, true)
	_, _, client := testCert(t, "client", ca, caKey, false)
	otherCA, otherKey, _ := testCert(t, "other", nil, nil, true)
	_, _, stranger := testCert(t, "stranger", otherCA, otherKey, false)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = &tls.Config{ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	ts.StartTLS()
	defer ts.Close()
	sum := sha256.Sum256(ts.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])
	ctx := context.Background()

	for name, certs := range map[string][]tls.Certificate{"no certificate": nil, "foreign CA": {stranger}} {
		r, _ := NewRemote(ts.URL, fp, "secret-key-0123456789", certs...)
		if _, err := r.Status(ctx); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r, _ := NewRemote(ts.URL, fp, "secret-key-0123456789", client)
	if _, err := r.Status(ctx); err != nil {
		t.Fatalf("client certificate rejected: %v", err)
	}
	bad, _ := NewRemote(ts.URL, fp, "wrong-key-0123456789", client)
	if _, err := bad.Status(ctx); err == nil {
		t.Fatal("certificate without the API key accepted")
	}
}
