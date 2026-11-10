// Package auth holds the fleet's key material: a master key from which the
// control plane signing key and every machine's TLS identity are derived,
// and signed, scoped tokens that machine servers verify with the control
// plane's public key.
//
// Everything secret is derived from the master key with HKDF-SHA256, so the
// control plane only has to keep (or be given) 32 bytes: machine
// fingerprints can be recomputed from master + machine id + expiry after the
// rest of the state is lost.
package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/transport"
)

// KeySize is the size of the master key and of derived seeds.
const KeySize = 32

// Environment variables and file name of the master key.
const (
	EnvMasterKey     = "CLOUDSOURCED_MASTER_KEY"      // base64 or hex encoded key
	EnvMasterKeyFile = "CLOUDSOURCED_MASTER_KEY_FILE" // file with the encoded key
	MasterKeyFile    = "master.key"                   // in the orchestrator state dir
)

// HKDF labels. Changing one invalidates every derived key.
const (
	hkdfSalt       = "cloudsourced/v1"
	infoSigner     = "control-plane-signer"
	infoMachineTLS = "machine-tls/"
)

// Master is the fleet master key.
type Master [KeySize]byte

// LoadMaster returns the master key: from $CLOUDSOURCED_MASTER_KEY, from the
// file named by $CLOUDSOURCED_MASTER_KEY_FILE, or from <dir>/master.key,
// which is created (0600) on first use. Several control plane replicas
// share a fleet by importing the same key via the environment or a file.
func LoadMaster(dir string) (Master, error) {
	if s := os.Getenv(EnvMasterKey); s != "" {
		return ParseMaster(s)
	}
	if p := os.Getenv(EnvMasterKeyFile); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return Master{}, fmt.Errorf("%s: %w", EnvMasterKeyFile, err)
		}
		return ParseMaster(string(b))
	}
	p := filepath.Join(dir, MasterKeyFile)
	if b, err := os.ReadFile(p); err == nil {
		return ParseMaster(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Master{}, err
	}
	var m Master
	if _, err := rand.Read(m[:]); err != nil {
		return Master{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Master{}, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		// Created concurrently: use the winner's key.
		b, err := os.ReadFile(p)
		if err != nil {
			return Master{}, err
		}
		return ParseMaster(string(b))
	}
	if err != nil {
		return Master{}, err
	}
	_, werr := f.WriteString(m.String() + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(p)
		return Master{}, werr
	}
	return m, nil
}

// ParseMaster decodes a master key (hex, base64 or base64url, padded or
// not; surrounding whitespace is ignored).
func ParseMaster(s string) (Master, error) {
	s = strings.TrimSpace(s)
	var m Master
	for _, dec := range []func(string) ([]byte, error){
		hex.DecodeString,
		base64.RawURLEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == KeySize {
			copy(m[:], b)
			return m, nil
		}
	}
	return m, errors.New("auth: master key must be 32 bytes, hex or base64 encoded")
}

// String encodes the key as unpadded base64url.
func (m Master) String() string { return EncodeKey(m[:]) }

func (m Master) derive(info string) []byte {
	b, err := hkdf.Key(sha256.New, m[:], []byte(hkdfSalt), info, KeySize)
	if err != nil {
		panic(err) // only fails for absurd lengths
	}
	return b
}

// Signer returns the control plane token signing key.
func (m Master) Signer() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(m.derive(infoSigner))
}

// SignerPublic returns the encoded public key machines verify tokens with.
func (m Master) SignerPublic() string {
	return EncodeKey(m.Signer().Public().(ed25519.PublicKey))
}

// MachineSeed returns the seed of a machine's TLS key. It is the only
// secret a machine receives.
func (m Master) MachineSeed(machineID string) []byte {
	return m.derive(infoMachineTLS + machineID)
}

// MachineFingerprint recomputes a machine's certificate fingerprint.
func (m Master) MachineFingerprint(machineID string, expires time.Time) (string, error) {
	id, err := MachineIdentity(m.MachineSeed(machineID), machineID, expires)
	if err != nil {
		return "", err
	}
	return id.Fingerprint, nil
}

// EncodeKey encodes key material (seeds, public keys) as unpadded base64url.
func EncodeKey(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeKey reverses EncodeKey and checks the length.
func DecodeKey(s string, size int) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	if len(b) != size {
		return nil, fmt.Errorf("auth: key has %d bytes, want %d", len(b), size)
	}
	return b, nil
}

// certGrace keeps a certificate valid past the machine's expiry while the
// server drains (clients pin the fingerprint and ignore validity anyway).
const certGrace = time.Hour

// MachineIdentity builds the deterministic self-signed certificate of a
// machine from its seed: Ed25519 key from the seed, serial from
// SHA-256(id), NotBefore the Unix epoch, NotAfter the machine expiry
// (truncated to seconds) plus one hour. Ed25519 signatures are
// deterministic, so the same inputs always yield the same DER and
// fingerprint.
func MachineIdentity(seed []byte, machineID string, expires time.Time) (*transport.Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("auth: bad machine seed")
	}
	if machineID == "" || expires.IsZero() {
		return nil, errors.New("auth: machine identity needs an id and an expiry")
	}
	key := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256([]byte("cloudsourced/machine-serial/" + machineID))
	serial := new(big.Int).SetBytes(sum[:15]) // < 2^120
	serial.SetBit(serial, 119, 1)             // positive and never zero
	name := "cs-" + machineID
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Unix(0, 0).UTC(),
		NotAfter:     time.Unix(expires.Unix(), 0).UTC().Add(certGrace),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	// Ed25519 signing reads no randomness; a fixed reader keeps any other
	// accidental use deterministic too.
	der, err := x509.CreateCertificate(bytes.NewReader(make([]byte, 4096)), tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	fp := sha256.Sum256(der)
	return &transport.Identity{
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
		Fingerprint: hex.EncodeToString(fp[:]),
	}, nil
}

// Machine server environment keys owned by this package.
const (
	// EnvTLSSeed (secret) is the encoded seed of the machine's TLS key.
	EnvTLSSeed = "CLOUDSOURCED_TLS_SEED"
	// EnvSignerPub (public) is the encoded control plane public key.
	EnvSignerPub = "CLOUDSOURCED_SIGNER_PUB"
	// EnvFingerprint (public) is the expected certificate fingerprint; the
	// server refuses to start when its derived identity differs.
	EnvFingerprint = "CLOUDSOURCED_FINGERPRINT"
)
