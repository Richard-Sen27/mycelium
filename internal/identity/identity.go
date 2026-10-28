// Package identity holds the hub certificate authority, node certificates,
// join tokens and agent tokens.
//
// Contract (owned by the orchestrator): the exported names and signatures in
// this file are the interface other packages build against. The implementing
// agent fills in the bodies and may add unexported helpers and further
// exported functions, but must not change these signatures without asking.
//
// Security rules for this package:
//   - private keys are generated locally and never serialised anywhere but the
//     caller-provided destination (file with mode 0600);
//   - all randomness comes from crypto/rand;
//   - comparisons of secrets use crypto/subtle;
//   - nothing here logs secrets or token strings.
package identity

import (
	"crypto"
	"crypto/x509"
	"errors"
	"strings"
	"time"
)

// TrustDomain is the SPIFFE trust domain of a hub, e.g. "hub.example.com" or
// "my-team". It is chosen at `mycel hub init` and never changes.
type TrustDomain string

// NodeSPIFFEID returns "spiffe://<td>/node/<name>".
// It does not validate its inputs; callers validate td and name first.
func NodeSPIFFEID(td TrustDomain, name string) string {
	return spiffeScheme + string(td) + "/node/" + name
}

// ParseNodeSPIFFEID splits a node SPIFFE ID into trust domain and node name.
func ParseNodeSPIFFEID(id string) (TrustDomain, string, error) {
	rest, ok := strings.CutPrefix(id, spiffeScheme)
	if !ok {
		return "", "", ErrInvalidID
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "node" {
		return "", "", ErrInvalidID
	}
	td := TrustDomain(parts[0])
	if !ValidTrustDomain(td) || !ValidNodeName(parts[2]) {
		return "", "", ErrInvalidID
	}
	return td, parts[2], nil
}

// ValidNodeName reports whether name is usable as a node name: 1–63 chars of
// [a-z0-9-], not starting or ending with "-".
func ValidNodeName(name string) bool {
	if len(name) < 1 || len(name) > 63 {
		return false
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// HubCAALPN is the ALPN marker nodes and CLIs list in every TLS hello to the
// hub (API and bus). A hub with an ACME certificate presents its CA-issued
// certificate to hellos that carry it; the marker is never negotiated as the
// connection's protocol (D026, internal/tunnel/doc.go).
const HubCAALPN = "mycel-ca"

// HubClientNextProtos is the ALPN list of node and CLI HTTPS clients of the
// hub: the marker, then the protocols actually spoken.
func HubClientNextProtos() []string { return []string{HubCAALPN, "h2", "http/1.1"} }

// Default lifetimes.
const (
	CALifetime         = 10 * 365 * 24 * time.Hour
	NodeCertLifetime   = 30 * 24 * time.Hour
	ServerCertLifetime = 365 * 24 * time.Hour
	AgentTokenLifetime = time.Hour
	JoinTokenDefault   = 15 * time.Minute
	JoinTokenMax       = 24 * time.Hour
)

// ---- Keys ----

// GenerateKey creates an ECDSA P-256 key and returns it with its PKCS#8 PEM.
func GenerateKey() (crypto.Signer, []byte, error) { return generateKey() }

// ParseKeyPEM parses a PKCS#8 or EC PEM private key.
func ParseKeyPEM(pemBytes []byte) (crypto.Signer, error) { return parseKeyPEM(pemBytes) }

// NewCSR builds a PKCS#10 CSR (DER) with CN=name, signed by key.
func NewCSR(key crypto.Signer, name string) ([]byte, error) { return newCSR(key, name) }

// ---- Certificate authority ----

// CA is the hub's root certificate authority.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	td   TrustDomain
}

// NewCA generates a new self-signed root CA for td with CALifetime.
func NewCA(td TrustDomain) (*CA, error) { return newCA(td, time.Now()) }

// LoadCA parses a CA from its certificate and key PEM.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) { return loadCA(certPEM, keyPEM) }

// TrustDomain returns the trust domain encoded in the CA certificate
// (URI SAN "spiffe://<td>").
func (c *CA) TrustDomain() TrustDomain { return c.td }

// CertPEM returns the CA certificate PEM.
func (c *CA) CertPEM() []byte { return encodeCertPEM(c.cert.Raw) }

// KeyPEM returns the CA private key PEM (PKCS#8).
func (c *CA) KeyPEM() []byte {
	p, err := encodeKeyPEM(c.key)
	if err != nil {
		// Unreachable: the key was generated or parsed as an ECDSA key,
		// which always marshals to PKCS#8.
		return nil
	}
	return p
}

// Fingerprint returns the SHA-256 of the CA certificate DER, lowercase hex.
func (c *CA) Fingerprint() string { return CertFingerprint(c.cert.Raw) }

// Pool returns a cert pool containing only this CA.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// SignNodeCSR verifies the CSR signature, ignores everything in the CSR except
// the public key, and issues a client certificate with URI SAN
// spiffe://<td>/node/<name>, CN=name, ExtKeyUsage ClientAuth, lifetime ttl.
// Returns the certificate PEM and its serial (lowercase hex).
func (c *CA) SignNodeCSR(csrDER []byte, name string, ttl time.Duration) (certPEM []byte, serial string, err error) {
	return c.signNodeCSR(csrDER, name, ttl, time.Now())
}

// IssueServerCert issues a TLS server certificate for the given DNS names and
// IP addresses (strings; IPs are detected), used by the hub API and bus
// listeners. Returns cert PEM and key PEM.
func (c *CA) IssueServerCert(hosts []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	return c.issueServerCert(hosts, ttl, time.Now())
}

// VerifyNodeCert checks that certPEM chains to pool, is a client cert, is
// within its validity period and carries exactly one node SPIFFE ID in td.
// It returns the parsed cert and the node name.
func VerifyNodeCert(pool *x509.CertPool, td TrustDomain, certPEM []byte) (*x509.Certificate, string, error) {
	return verifyNodeCertAt(pool, td, certPEM, time.Now())
}

// NodeNameFromCert extracts the node name from an already-verified certificate.
func NodeNameFromCert(td TrustDomain, cert *x509.Certificate) (string, error) {
	return nodeNameFromCert(td, cert)
}

// CertFingerprint returns the SHA-256 of DER as lowercase hex.
func CertFingerprint(der []byte) string { return sha256Hex(der) }

// FingerprintPEM parses the first certificate in pemBytes and fingerprints it.
func FingerprintPEM(pemBytes []byte) (string, error) {
	cert, err := parseFirstCertPEM(pemBytes)
	if err != nil {
		return "", err
	}
	return CertFingerprint(cert.Raw), nil
}

// SerialHex formats a certificate serial as lowercase hex.
func SerialHex(cert *x509.Certificate) string { return cert.SerialNumber.Text(16) }

// ---- Join tokens ----

// JoinTokenPrefix is the leading marker of a join token string.
const JoinTokenPrefix = "mcl1_"

// JoinToken is the decoded form of a join token string (docs/decisions.md D005).
type JoinToken struct {
	HubURL        string
	ID            string // 16 random bytes, hex
	Secret        []byte // 32 random bytes
	CAFingerprint string
}

// NewJoinToken creates a token with a fresh ID and secret.
func NewJoinToken(hubURL, caFingerprint string) (*JoinToken, error) {
	return newJoinToken(hubURL, caFingerprint)
}

// String encodes the token as "mcl1_" + base64url(JoinTokenPayload).
func (t *JoinToken) String() string { return t.encode() }

// SecretHash returns sha256(secret) as lowercase hex; this is what the hub stores.
func (t *JoinToken) SecretHash() string { return HashSecret(t.Secret) }

// ParseJoinToken decodes a token string. It validates the prefix, the field
// lengths and that hub_url is an https or http URL.
func ParseJoinToken(s string) (*JoinToken, error) { return parseJoinToken(s) }

// HashSecret returns sha256(secret) as lowercase hex.
func HashSecret(secret []byte) string { return sha256Hex(secret) }

// ConstantTimeEqualHex compares two hex digests in constant time.
func ConstantTimeEqualHex(a, b string) bool { return constantTimeEqualHex(a, b) }

// ---- Agent tokens ----

// AgentClaims are the contents of an agent token.
type AgentClaims struct {
	AgentID   string // "agent:<name>@<node>"
	NodeName  string
	SandboxID string // empty for trusted-host agents
	OwnerID   string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// AgentTokenSigner issues and verifies agent tokens. Tokens are
// "mca1_" + base64url(json(claims)) + "." + base64url(hmac-sha256(key, payload)).
type AgentTokenSigner struct {
	key []byte
}

// AgentTokenPrefix is the leading marker of an agent token string.
const AgentTokenPrefix = "mca1_"

// NewAgentTokenSigner wraps a 32-byte HMAC key. Shorter keys are rejected.
func NewAgentTokenSigner(key []byte) (*AgentTokenSigner, error) {
	return newAgentTokenSigner(key)
}

// NewAgentTokenKey returns 32 random bytes.
func NewAgentTokenKey() ([]byte, error) { return randomBytes(agentKeyLen) }

// Issue signs claims. IssuedAt and ExpiresAt are set by Issue: now and now+ttl.
func (s *AgentTokenSigner) Issue(claims AgentClaims, ttl time.Duration) (string, error) {
	return s.issueAt(claims, ttl, time.Now())
}

// Verify checks signature and expiry and returns the claims. Callers must
// additionally check that claims.NodeName matches the presenting node
// connection: that is what binds a token to its sandbox.
func (s *AgentTokenSigner) Verify(token string) (*AgentClaims, error) {
	return s.verifyAt(token, time.Now())
}

// ---- Errors ----

var (
	ErrInvalidToken  = errors.New("identity: invalid token")
	ErrExpired       = errors.New("identity: expired")
	ErrBadSignature  = errors.New("identity: bad signature")
	ErrInvalidName   = errors.New("identity: invalid node name")
	ErrInvalidCert   = errors.New("identity: invalid certificate")
	ErrWrongTrustDom = errors.New("identity: wrong trust domain")
)

// Additional errors added by the implementation (not in the original contract).
var (
	// ErrInvalidID reports a malformed node SPIFFE ID.
	ErrInvalidID = errors.New("identity: invalid SPIFFE ID")
	// ErrInvalidKey reports a malformed or unsupported private or public key.
	ErrInvalidKey = errors.New("identity: invalid key")
	// ErrInvalidTrustDomain reports a malformed trust domain.
	ErrInvalidTrustDomain = errors.New("identity: invalid trust domain")
	// ErrInvalidArgument reports a bad parameter such as a non-positive ttl
	// or a malformed hub URL.
	ErrInvalidArgument = errors.New("identity: invalid argument")
)
