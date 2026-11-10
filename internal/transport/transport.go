// Package transport provides QUIC listeners and dialers with pinned
// self-signed certificates.
package transport

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
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/traumventures/cloudsourced/internal/proto"
	"github.com/traumventures/cloudsourced/internal/telemetry"
)

// Identity is a server certificate with its key in PEM form.
type Identity struct {
	CertPEM     string
	KeyPEM      string
	Fingerprint string // hex SHA-256 of the DER certificate
}

// NewIdentity creates a self-signed certificate valid for lifetime.
func NewIdentity(name string, lifetime time.Duration) (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(lifetime + time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &Identity{
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})),
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 60 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		MaxIncomingStreams:             4096,
		MaxIncomingUniStreams:          -1,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
	}
}

// Listen starts a QUIC listener with the given identity.
func Listen(addr string, id *Identity) (*quic.Listener, error) {
	cert, err := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{proto.ALPN}, MinVersion: tls.VersionTLS13}
	return quic.ListenAddr(addr, tc, quicConfig())
}

// Dial connects to addr and verifies the server certificate fingerprint.
func Dial(ctx context.Context, addr, fingerprint string) (*quic.Conn, error) {
	tc := &tls.Config{
		InsecureSkipVerify: true, // replaced by fingerprint pinning below
		NextProtos:         []string{proto.ALPN},
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != fingerprint {
				return fmt.Errorf("server certificate fingerprint mismatch")
			}
			return nil
		},
	}
	udp, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: pc}
	conn, err := tr.Dial(ctx, udp, tc, quicConfig())
	if err != nil {
		tr.Close()
		pc.Close()
		return nil, err
	}
	go func() {
		<-conn.Context().Done()
		tr.Close()
		pc.Close()
	}()
	return conn, nil
}

// OpenStream opens a stream and sends Hello.
func OpenStream(ctx context.Context, conn *quic.Conn, token, kind string) (*quic.Stream, *proto.Conn, error) {
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, err
	}
	pc := proto.NewConn(s)
	if err := pc.Send(proto.THello, proto.Hello{Token: token, Kind: kind, TraceParent: telemetry.Inject(ctx)}); err != nil {
		s.CancelRead(0)
		s.Close()
		return nil, nil, err
	}
	return s, pc, nil
}
