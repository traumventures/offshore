package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Scope limits what a token may do on a machine.
type Scope string

const (
	// ScopeSession allows attaching roots, running and fetching outputs,
	// bound to one lease.
	ScopeSession Scope = "session"
	// ScopeProbe allows reading the machine status.
	ScopeProbe Scope = "probe"
	// ScopeAdmin allows everything, including destructive operations.
	ScopeAdmin Scope = "admin"
)

// TokenVersion is the claims format version.
const TokenVersion = 1

// Claims are the signed contents of a token.
type Claims struct {
	V       int    `json:"v"`
	Scope   Scope  `json:"scope"`
	Machine string `json:"machine"`
	Lease   string `json:"lease,omitempty"`
	Exp     int64  `json:"exp"`
	Iat     int64  `json:"iat"`
}

// signContext separates token signatures from any other use of the key.
const signContext = "cloudsourced-token-v1."

// Sign encodes and signs claims: base64url(JSON) "." base64url(signature).
func Sign(key ed25519.PrivateKey, c Claims) string {
	if c.V == 0 {
		c.V = TokenVersion
	}
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	sig := ed25519.Sign(key, []byte(signContext+payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// NewToken signs a token for scope on machine, valid until until.
func NewToken(key ed25519.PrivateKey, scope Scope, machine, lease string, until time.Time) string {
	return Sign(key, Claims{V: TokenVersion, Scope: scope, Machine: machine, Lease: lease, Exp: until.Unix(), Iat: time.Now().Unix()})
}

// ErrInvalidToken wraps every rejection.
var ErrInvalidToken = errors.New("invalid token")

// MaxTokenLen bounds the accepted token size.
const MaxTokenLen = 2048

// DefaultSkew is the tolerated clock difference between control plane and
// machine.
const DefaultSkew = 2 * time.Minute

// Verifier checks tokens on a machine.
type Verifier struct {
	Pub     ed25519.PublicKey
	Machine string
	Skew    time.Duration    // 0: DefaultSkew
	Now     func() time.Time // nil: time.Now
}

// Verify checks signature, version, machine, scope shape and expiry and
// returns the claims. Which scope an operation needs is the caller's
// decision (see Claims.Allows).
func (v Verifier) Verify(tok string) (Claims, error) {
	var c Claims
	if len(v.Pub) != ed25519.PublicKeySize {
		return c, fmt.Errorf("%w: no verification key", ErrInvalidToken)
	}
	if len(tok) > MaxTokenLen {
		return c, fmt.Errorf("%w: too long", ErrInvalidToken)
	}
	payload, sigS, ok := strings.Cut(tok, ".")
	if !ok {
		return c, fmt.Errorf("%w: malformed", ErrInvalidToken)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigS)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, fmt.Errorf("%w: malformed signature", ErrInvalidToken)
	}
	if !ed25519.Verify(v.Pub, []byte(signContext+payload), sig) {
		return c, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return Claims{}, fmt.Errorf("%w: malformed claims", ErrInvalidToken)
	}
	if c.V != TokenVersion {
		return c, fmt.Errorf("%w: unsupported version %d", ErrInvalidToken, c.V)
	}
	if c.Machine != v.Machine {
		return c, fmt.Errorf("%w: issued for another machine", ErrInvalidToken)
	}
	switch c.Scope {
	case ScopeSession:
		if c.Lease == "" {
			return c, fmt.Errorf("%w: session token without lease", ErrInvalidToken)
		}
	case ScopeProbe, ScopeAdmin:
	default:
		return c, fmt.Errorf("%w: unknown scope %q", ErrInvalidToken, c.Scope)
	}
	skew := v.Skew
	if skew == 0 {
		skew = DefaultSkew
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if now.After(time.Unix(c.Exp, 0).Add(skew)) {
		return c, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if time.Unix(c.Iat, 0).After(now.Add(skew)) {
		return c, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	return c, nil
}

// Allows reports whether the claims grant scope want. Admin grants every
// scope.
func (c Claims) Allows(want Scope) bool {
	return c.Scope == want || c.Scope == ScopeAdmin
}

// AllowsLease reports whether the claims may act on lease: a session token
// only on its own lease, an admin token on any.
func (c Claims) AllowsLease(lease string) bool {
	if c.Scope == ScopeAdmin {
		return true
	}
	return c.Scope == ScopeSession && lease != "" && c.Lease == lease
}
