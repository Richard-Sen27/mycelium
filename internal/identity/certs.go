package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

const (
	spiffeScheme   = "spiffe://"
	pemTypeCert    = "CERTIFICATE"
	pemTypePKCS8   = "PRIVATE KEY"
	pemTypeECKey   = "EC PRIVATE KEY"
	clockSkew      = time.Minute
	maxTrustDomain = 255
	maxCSRLen      = 4096
)

// ValidTrustDomain reports whether td is a usable SPIFFE trust domain:
// 1–255 chars of [a-z0-9.-_].
func ValidTrustDomain(td TrustDomain) bool {
	if len(td) < 1 || len(td) > maxTrustDomain {
		return false
	}
	for i := 0; i < len(td); i++ {
		c := td[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' && c != '_' {
			return false
		}
	}
	return true
}

// ---- keys ----

func generateKey() (crypto.Signer, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("identity: generate key: %w", err)
	}
	p, err := encodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return key, p, nil
}

func encodeKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrInvalidKey, err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypePKCS8, Bytes: der}), nil
}

func parseKeyPEM(pemBytes []byte) (crypto.Signer, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("%w: no private key PEM block", ErrInvalidKey)
		}
		switch block.Type {
		case pemTypePKCS8:
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("%w: not an ECDSA key", ErrInvalidKey)
			}
			return checkECDSAKey(ek)
		case pemTypeECKey:
			ek, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
			}
			return checkECDSAKey(ek)
		}
	}
}

func checkECDSAKey(k *ecdsa.PrivateKey) (crypto.Signer, error) {
	if !supportedCurve(k.Curve) {
		return nil, fmt.Errorf("%w: unsupported curve", ErrInvalidKey)
	}
	return k, nil
}

func supportedCurve(c elliptic.Curve) bool {
	return c == elliptic.P256() || c == elliptic.P384()
}

// checkPublicKey accepts only ECDSA P-256 / P-384 public keys.
func checkPublicKey(pub any) error {
	k, ok := pub.(*ecdsa.PublicKey)
	if !ok || !supportedCurve(k.Curve) {
		return fmt.Errorf("%w: public key must be ECDSA P-256 or P-384", ErrInvalidKey)
	}
	return nil
}

func newCSR(key crypto.Signer, name string) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: nil key", ErrInvalidKey)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("identity: create CSR: %w", err)
	}
	return der, nil
}

// ---- helpers ----

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der})
}

// parseFirstCertPEM parses the first CERTIFICATE block, skipping other blocks.
func parseFirstCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("%w: no CERTIFICATE PEM block", ErrInvalidCert)
		}
		if block.Type != pemTypeCert {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCert, err)
		}
		return cert, nil
	}
}

// randomSerial returns a uniformly random serial in [1, 2^128).
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	limit.Sub(limit, big.NewInt(1))
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("identity: serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

// ---- CA ----

func newCA(td TrustDomain, now time.Time) (*CA, error) {
	if !ValidTrustDomain(td) {
		return nil, ErrInvalidTrustDomain
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Mycelium CA " + string(td)},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CALifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: string(td)}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("identity: create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("identity: parse CA cert: %w", err)
	}
	return &CA{cert: cert, key: key, td: td}, nil
}

func loadCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := parseFirstCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("%w: not a CA certificate", ErrInvalidCert)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, fmt.Errorf("%w: not self-signed: %v", ErrInvalidCert, err)
	}
	td, err := caTrustDomain(cert)
	if err != nil {
		return nil, err
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("%w: key does not match CA certificate", ErrInvalidKey)
	}
	return &CA{cert: cert, key: key, td: td}, nil
}

// caTrustDomain reads the single URI SAN "spiffe://<td>" of a CA certificate.
func caTrustDomain(cert *x509.Certificate) (TrustDomain, error) {
	if len(cert.URIs) != 1 {
		return "", fmt.Errorf("%w: CA must carry exactly one URI SAN", ErrInvalidCert)
	}
	u := cert.URIs[0]
	if u.String() != spiffeScheme+u.Host {
		return "", fmt.Errorf("%w: CA URI SAN is not spiffe://<trust-domain>", ErrInvalidCert)
	}
	td := TrustDomain(u.Host)
	if !ValidTrustDomain(td) {
		return "", fmt.Errorf("%w: %w", ErrInvalidCert, ErrInvalidTrustDomain)
	}
	return td, nil
}

// clampNotAfter keeps an issued certificate from outliving the CA.
func (c *CA) clampNotAfter(t time.Time) time.Time {
	if t.After(c.cert.NotAfter) {
		return c.cert.NotAfter
	}
	return t
}

func (c *CA) signNodeCSR(csrDER []byte, name string, ttl time.Duration, now time.Time) ([]byte, string, error) {
	if !ValidNodeName(name) {
		return nil, "", ErrInvalidName
	}
	if ttl <= 0 || ttl > NodeCertLifetime {
		return nil, "", fmt.Errorf("%w: ttl must be in (0, %v]", ErrInvalidArgument, NodeCertLifetime)
	}
	if len(csrDER) > maxCSRLen {
		return nil, "", fmt.Errorf("%w: CSR larger than %d bytes", ErrInvalidArgument, maxCSRLen)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, "", fmt.Errorf("identity: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("%w: CSR signature: %v", ErrBadSignature, err)
	}
	if err := checkPublicKey(csr.PublicKey); err != nil {
		return nil, "", err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, "", err
	}
	id, err := url.Parse(NodeSPIFFEID(c.td, name))
	if err != nil {
		return nil, "", fmt.Errorf("identity: node SPIFFE ID: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     c.clampNotAfter(now.Add(ttl)),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, "", fmt.Errorf("identity: sign node cert: %w", err)
	}
	return encodeCertPEM(der), serial.Text(16), nil
}

func (c *CA) issueServerCert(hosts []string, ttl time.Duration, now time.Time) ([]byte, []byte, error) {
	if len(hosts) == 0 {
		return nil, nil, fmt.Errorf("%w: no hosts", ErrInvalidArgument)
	}
	if ttl <= 0 || ttl > ServerCertLifetime {
		return nil, nil, fmt.Errorf("%w: ttl must be in (0, %v]", ErrInvalidArgument, ServerCertLifetime)
	}
	var dns []string
	var ips []net.IP
	for _, h := range hosts {
		if h == "" {
			return nil, nil, fmt.Errorf("%w: empty host", ErrInvalidArgument)
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	key, keyPEM, err := generateKey()
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     c.clampNotAfter(now.Add(ttl)),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("identity: sign server cert: %w", err)
	}
	return encodeCertPEM(der), keyPEM, nil
}

func verifyNodeCertAt(pool *x509.CertPool, td TrustDomain, certPEM []byte, now time.Time) (*x509.Certificate, string, error) {
	if pool == nil {
		return nil, "", fmt.Errorf("%w: nil pool", ErrInvalidArgument)
	}
	cert, err := parseFirstCertPEM(certPEM)
	if err != nil {
		return nil, "", err
	}
	if cert.IsCA {
		return nil, "", fmt.Errorf("%w: CA certificate presented as node certificate", ErrInvalidCert)
	}
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, "", fmt.Errorf("%w: outside validity period", ErrInvalidCert)
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidCert, err)
	}
	// Go treats an empty EKU list or ExtKeyUsageAny as "any usage"; a node
	// certificate must name ClientAuth explicitly.
	hasClientAuth := false
	for _, u := range cert.ExtKeyUsage {
		switch u {
		case x509.ExtKeyUsageAny:
			return nil, "", fmt.Errorf("%w: ExtKeyUsageAny not allowed", ErrInvalidCert)
		case x509.ExtKeyUsageClientAuth:
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		return nil, "", fmt.Errorf("%w: missing ClientAuth extended key usage", ErrInvalidCert)
	}
	name, err := nodeNameFromCert(td, cert)
	if err != nil {
		return nil, "", err
	}
	return cert, name, nil
}

func nodeNameFromCert(td TrustDomain, cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", fmt.Errorf("%w: nil certificate", ErrInvalidCert)
	}
	if len(cert.URIs) != 1 {
		return "", fmt.Errorf("%w: need exactly one URI SAN", ErrInvalidCert)
	}
	if len(cert.DNSNames) != 0 || len(cert.IPAddresses) != 0 || len(cert.EmailAddresses) != 0 {
		return "", fmt.Errorf("%w: unexpected SANs on node certificate", ErrInvalidCert)
	}
	gotTD, name, err := ParseNodeSPIFFEID(cert.URIs[0].String())
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidCert, err)
	}
	if gotTD != td {
		return "", ErrWrongTrustDom
	}
	return name, nil
}
