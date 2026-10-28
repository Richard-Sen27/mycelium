package identity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const testTD TrustDomain = "hub.example.com"

func mustCA(t *testing.T, td TrustDomain) *CA {
	t.Helper()
	ca, err := NewCA(td)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return ca
}

func mustCSR(t *testing.T, name string) []byte {
	t.Helper()
	key, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	csr, err := NewCSR(key, name)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	return csr
}

func mustNodeCert(t *testing.T, ca *CA, name string) []byte {
	t.Helper()
	certPEM, _, err := ca.SignNodeCSR(mustCSR(t, name), name, NodeCertLifetime)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}
	return certPEM
}

// customLeaf issues a leaf directly from the CA's key, starting from a valid
// node certificate template that mod then alters.
func customLeaf(t *testing.T, ca *CA, mod func(*x509.Certificate)) []byte {
	t.Helper()
	key, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := url.Parse(NodeSPIFFEID(ca.td, "n1"))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "n1"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id},
	}
	mod(tmpl)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return encodeCertPEM(der)
}

func parsePEMCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	c, err := parseFirstCertPEM(p)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

// ---- keys ----

func TestKeyRoundTrip(t *testing.T) {
	key, keyPEM, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ek, ok := key.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		t.Fatalf("want ECDSA P-256, got %T", key)
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		t.Fatalf("want PRIVATE KEY PEM, got %+v", blk)
	}
	ecDER, err := x509.MarshalECPrivateKey(ek)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER})

	tests := []struct {
		name    string
		in      []byte
		wantErr bool
	}{
		{"pkcs8", keyPEM, false},
		{"ec", ecPEM, false},
		{"garbage", []byte("not a key"), true},
		{"empty", nil, true},
		{"wrong type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}), true},
		{"corrupt pkcs8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseKeyPEM(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidKey) {
					t.Fatalf("want ErrInvalidKey, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !ek.Equal(got) {
				t.Fatal("parsed key differs")
			}
		})
	}
}

// ---- CA ----

func TestCARoundTrip(t *testing.T) {
	ca := mustCA(t, testTD)
	if ca.TrustDomain() != testTD {
		t.Fatalf("td = %q", ca.TrustDomain())
	}
	cert := parsePEMCert(t, ca.CertPEM())
	if !cert.IsCA || !cert.BasicConstraintsValid {
		t.Fatal("not a CA")
	}
	if cert.Subject.CommonName != "Mycelium CA "+string(testTD) {
		t.Fatalf("CN = %q", cert.Subject.CommonName)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://"+string(testTD) {
		t.Fatalf("URIs = %v", cert.URIs)
	}
	wantKU := x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
	if cert.KeyUsage != wantKU {
		t.Fatalf("KeyUsage = %v", cert.KeyUsage)
	}
	if cert.SerialNumber.Sign() <= 0 || cert.SerialNumber.BitLen() > 128 {
		t.Fatalf("serial = %v", cert.SerialNumber)
	}
	if life := cert.NotAfter.Sub(cert.NotBefore); life < CALifetime || life > CALifetime+2*time.Minute {
		t.Fatalf("lifetime = %v", life)
	}

	loaded, err := LoadCA(ca.CertPEM(), ca.KeyPEM())
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if loaded.TrustDomain() != testTD {
		t.Fatalf("loaded td = %q", loaded.TrustDomain())
	}
	if loaded.Fingerprint() != ca.Fingerprint() {
		t.Fatal("fingerprint changed")
	}
	if !isLowerHex(ca.Fingerprint(), 64) {
		t.Fatalf("fingerprint %q", ca.Fingerprint())
	}
	fp, err := FingerprintPEM(ca.CertPEM())
	if err != nil || fp != ca.Fingerprint() {
		t.Fatalf("FingerprintPEM = %q, %v", fp, err)
	}

	// Certificates issued by the loaded CA verify against the original pool.
	certPEM := mustNodeCert(t, loaded, "n1")
	if _, name, err := VerifyNodeCert(ca.Pool(), testTD, certPEM); err != nil || name != "n1" {
		t.Fatalf("verify = %q, %v", name, err)
	}
}

func TestCAPathLenAndClamp(t *testing.T) {
	// A CA that expires in one hour: issued certs must not outlive it.
	ca, err := newCA(testTD, time.Now().Add(-CALifetime+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ca.cert.MaxPathLen != 0 || !ca.cert.MaxPathLenZero {
		t.Fatalf("MaxPathLen = %d zero=%v", ca.cert.MaxPathLen, ca.cert.MaxPathLenZero)
	}
	node := parsePEMCert(t, mustNodeCert(t, ca, "n1"))
	if !node.NotAfter.Equal(ca.cert.NotAfter) {
		t.Fatalf("node NotAfter %v not clamped to CA %v", node.NotAfter, ca.cert.NotAfter)
	}
	srvPEM, _, err := ca.IssueServerCert([]string{"hub"}, ServerCertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if srv := parsePEMCert(t, srvPEM); !srv.NotAfter.Equal(ca.cert.NotAfter) {
		t.Fatalf("server NotAfter %v not clamped to CA %v", srv.NotAfter, ca.cert.NotAfter)
	}
}

func TestLoadCAErrors(t *testing.T) {
	ca := mustCA(t, testTD)
	other := mustCA(t, testTD)
	leaf := mustNodeCert(t, ca, "n1")
	tests := []struct {
		name    string
		cert    []byte
		key     []byte
		wantErr error
	}{
		{"mismatched key", ca.CertPEM(), other.KeyPEM(), ErrInvalidKey},
		{"leaf as CA", leaf, ca.KeyPEM(), ErrInvalidCert},
		{"garbage cert", []byte("x"), ca.KeyPEM(), ErrInvalidCert},
		{"garbage key", ca.CertPEM(), []byte("x"), ErrInvalidKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadCA(tc.cert, tc.key); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestNewCAInvalidTrustDomain(t *testing.T) {
	for _, td := range []TrustDomain{"", "Hub", "a/b", "a b", "spiffe://x"} {
		if _, err := NewCA(td); !errors.Is(err, ErrInvalidTrustDomain) {
			t.Errorf("NewCA(%q): want ErrInvalidTrustDomain, got %v", td, err)
		}
	}
}

// ---- node certs ----

func TestSignAndVerifyNodeCert(t *testing.T) {
	ca := mustCA(t, testTD)
	key, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key, "ignored-cn")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, err := ca.SignNodeCSR(csr, "gpu-box", NodeCertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	cert, name, err := VerifyNodeCert(ca.Pool(), testTD, certPEM)
	if err != nil {
		t.Fatalf("VerifyNodeCert: %v", err)
	}
	if name != "gpu-box" {
		t.Fatalf("name = %q", name)
	}
	if cert.Subject.CommonName != "gpu-box" {
		t.Fatalf("CN = %q (CSR subject must be ignored)", cert.Subject.CommonName)
	}
	if serial != SerialHex(cert) || serial != strings.ToLower(serial) {
		t.Fatalf("serial %q vs %q", serial, SerialHex(cert))
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://hub.example.com/node/gpu-box" {
		t.Fatalf("URIs = %v", cert.URIs)
	}
	if len(cert.DNSNames)+len(cert.IPAddresses)+len(cert.EmailAddresses) != 0 {
		t.Fatal("unexpected SANs")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("EKU = %v", cert.ExtKeyUsage)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("KU = %v", cert.KeyUsage)
	}
	if !key.Public().(*ecdsa.PublicKey).Equal(cert.PublicKey) {
		t.Fatal("cert key is not the CSR key")
	}
	if n, err := NodeNameFromCert(testTD, cert); err != nil || n != "gpu-box" {
		t.Fatalf("NodeNameFromCert = %q, %v", n, err)
	}
	if _, err := NodeNameFromCert("other", cert); !errors.Is(err, ErrWrongTrustDom) {
		t.Fatalf("want ErrWrongTrustDom, got %v", err)
	}
}

func TestSignNodeCSRRejects(t *testing.T) {
	ca := mustCA(t, testTD)
	good := mustCSR(t, "n1")
	tampered := bytes.Clone(good)
	tampered[len(tampered)-1] ^= 0xff
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaCSR, err := NewCSR(rsaKey, "n1")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		csr     []byte
		node    string
		ttl     time.Duration
		wantErr error
	}{
		{"tampered signature", tampered, "n1", time.Hour, ErrBadSignature},
		{"invalid name", good, "Bad_Name", time.Hour, ErrInvalidName},
		{"empty name", good, "", time.Hour, ErrInvalidName},
		{"zero ttl", good, "n1", 0, ErrInvalidArgument},
		{"ttl above NodeCertLifetime", good, "n1", NodeCertLifetime + time.Second, ErrInvalidArgument},
		{"rsa key", rsaCSR, "n1", time.Hour, ErrInvalidKey},
		{"oversized csr", make([]byte, maxCSRLen+1), "n1", time.Hour, ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ca.SignNodeCSR(tc.csr, tc.node, tc.ttl); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
	if _, _, err := ca.SignNodeCSR([]byte("garbage"), "n1", time.Hour); err == nil {
		t.Fatal("garbage CSR accepted")
	}
}

func TestVerifyNodeCertRejects(t *testing.T) {
	ca := mustCA(t, testTD)
	otherCA := mustCA(t, testTD)
	otherTD := mustCA(t, "other.example.com")
	now := time.Now()

	expired, _, err := ca.signNodeCSR(mustCSR(t, "n1"), "n1", time.Hour, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	future, _, err := ca.signNodeCSR(mustCSR(t, "n1"), "n1", time.Hour, now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	serverCert, _, err := ca.IssueServerCert([]string{"hub.example.com"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wrongTD := mustNodeCert(t, otherTD, "n1")
	id1, _ := url.Parse(NodeSPIFFEID(testTD, "n1"))
	id2, _ := url.Parse(NodeSPIFFEID(testTD, "n2"))
	twoURIs := customLeaf(t, ca, func(c *x509.Certificate) { c.URIs = []*url.URL{id1, id2} })
	noEKU := customLeaf(t, ca, func(c *x509.Certificate) { c.ExtKeyUsage = nil })
	anyEKU := customLeaf(t, ca, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny} })
	clientAndAny := customLeaf(t, ca, func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageAny}
	})
	dnsSAN := customLeaf(t, ca, func(c *x509.Certificate) { c.DNSNames = []string{"n1.example.com"} })
	// Sanity: the unmodified custom leaf is accepted.
	if _, name, err := VerifyNodeCert(ca.Pool(), testTD, customLeaf(t, ca, func(*x509.Certificate) {})); err != nil || name != "n1" {
		t.Fatalf("baseline custom leaf rejected: %q %v", name, err)
	}

	tests := []struct {
		name    string
		pool    *x509.CertPool
		td      TrustDomain
		cert    []byte
		wantErr error
	}{
		{"wrong trust domain arg", ca.Pool(), "other.example.com", mustNodeCert(t, ca, "n1"), ErrWrongTrustDom},
		{"cert from other trust domain CA, trusted pool", otherTD.Pool(), testTD, wrongTD, ErrWrongTrustDom},
		{"other CA", ca.Pool(), testTD, mustNodeCert(t, otherCA, "n1"), ErrInvalidCert},
		{"expired", ca.Pool(), testTD, expired, ErrInvalidCert},
		{"not yet valid", ca.Pool(), testTD, future, ErrInvalidCert},
		{"server cert", ca.Pool(), testTD, serverCert, ErrInvalidCert},
		{"CA cert", ca.Pool(), testTD, ca.CertPEM(), ErrInvalidCert},
		{"garbage", ca.Pool(), testTD, []byte("nope"), ErrInvalidCert},
		{"two URI SANs", ca.Pool(), testTD, twoURIs, ErrInvalidCert},
		{"no EKU", ca.Pool(), testTD, noEKU, ErrInvalidCert},
		{"EKU any", ca.Pool(), testTD, anyEKU, ErrInvalidCert},
		{"EKU client+any", ca.Pool(), testTD, clientAndAny, ErrInvalidCert},
		{"extra DNS SAN", ca.Pool(), testTD, dnsSAN, ErrInvalidCert},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := VerifyNodeCert(tc.pool, tc.td, tc.cert); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}

	// A valid cert verified after its expiry is rejected.
	good := mustNodeCert(t, ca, "n1")
	if _, _, err := verifyNodeCertAt(ca.Pool(), testTD, good, now.Add(NodeCertLifetime+time.Hour)); !errors.Is(err, ErrInvalidCert) {
		t.Fatalf("expired at verify time: got %v", err)
	}
}

func TestIssueServerCert(t *testing.T) {
	ca := mustCA(t, testTD)
	certPEM, keyPEM, err := ca.IssueServerCert([]string{"hub.example.com", "127.0.0.1", "::1", "localhost"}, ServerCertLifetime)
	if err != nil {
		t.Fatal(err)
	}
	cert := parsePEMCert(t, certPEM)
	if cert.Subject.CommonName != "hub.example.com" {
		t.Fatalf("CN = %q", cert.Subject.CommonName)
	}
	if strings.Join(cert.DNSNames, ",") != "hub.example.com,localhost" {
		t.Fatalf("DNS = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 2 || !cert.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) || !cert.IPAddresses[1].Equal(net.ParseIP("::1")) {
		t.Fatalf("IPs = %v", cert.IPAddresses)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("EKU = %v", cert.ExtKeyUsage)
	}
	key, err := ParseKeyPEM(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Public().(*ecdsa.PublicKey).Equal(cert.PublicKey) {
		t.Fatal("key does not match cert")
	}
	for _, h := range []string{"hub.example.com", "127.0.0.1", "localhost"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: h}); err != nil {
			t.Errorf("verify for %s: %v", h, err)
		}
	}
	if _, _, err := ca.IssueServerCert([]string{"hub"}, ServerCertLifetime+time.Second); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ttl above ServerCertLifetime: got %v", err)
	}
	if _, _, err := ca.IssueServerCert(nil, time.Hour); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("no hosts: got %v", err)
	}
}

func TestFingerprintPEMSkipsOtherBlocks(t *testing.T) {
	ca := mustCA(t, testTD)
	in := append(ca.KeyPEM(), ca.CertPEM()...)
	fp, err := FingerprintPEM(in)
	if err != nil || fp != ca.Fingerprint() {
		t.Fatalf("got %q, %v", fp, err)
	}
	if _, err := FingerprintPEM(ca.KeyPEM()); !errors.Is(err, ErrInvalidCert) {
		t.Fatalf("no cert: got %v", err)
	}
}

// ---- names and SPIFFE IDs ----

func TestValidNodeName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"a", true},
		{"gpu-box", true},
		{"node1", true},
		{"0", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"-a", false},
		{"a-", false},
		{"-", false},
		{"GPU", false},
		{"a_b", false},
		{"a.b", false},
		{"a b", false},
		{"a/b", false},
		{"é", false},
	}
	for _, tc := range tests {
		if got := ValidNodeName(tc.name); got != tc.want {
			t.Errorf("ValidNodeName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseNodeSPIFFEID(t *testing.T) {
	if got := NodeSPIFFEID(testTD, "gpu-box"); got != "spiffe://hub.example.com/node/gpu-box" {
		t.Fatalf("NodeSPIFFEID = %q", got)
	}
	tests := []struct {
		id       string
		wantTD   TrustDomain
		wantName string
		ok       bool
	}{
		{"spiffe://hub.example.com/node/gpu-box", "hub.example.com", "gpu-box", true},
		{"spiffe://my-team/node/a", "my-team", "a", true},
		{"spiffe://hub/node/a/b", "", "", false},
		{"spiffe://hub/node/", "", "", false},
		{"spiffe:///node/a", "", "", false},
		{"spiffe://hub/agent/a", "", "", false},
		{"spiffe://hub/node", "", "", false},
		{"spiffe://hub", "", "", false},
		{"https://hub/node/a", "", "", false},
		{"SPIFFE://hub/node/a", "", "", false},
		{"spiffe://Hub/node/a", "", "", false},
		{"spiffe://hub/node/A", "", "", false},
		{"spiffe://hub/node/a?x=1", "", "", false},
		{"spiffe://hub/node/a#f", "", "", false},
		{"spiffe://u@hub/node/a", "", "", false},
		{"spiffe://hub:443/node/a", "", "", false},
		{"spiffe://hub//node/a", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		td, name, err := ParseNodeSPIFFEID(tc.id)
		if tc.ok {
			if err != nil || td != tc.wantTD || name != tc.wantName {
				t.Errorf("Parse(%q) = %q, %q, %v", tc.id, td, name, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("Parse(%q): want ErrInvalidID, got %q %q %v", tc.id, td, name, err)
		}
	}
}

// ---- join tokens ----

func TestJoinTokenRoundTrip(t *testing.T) {
	ca := mustCA(t, testTD)
	tok, err := NewJoinToken("https://hub.example.com:7777", ca.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Secret) != 32 || !isLowerHex(tok.ID, 32) {
		t.Fatalf("bad token fields: id=%q secret=%d", tok.ID, len(tok.Secret))
	}
	s := tok.String()
	if !strings.HasPrefix(s, JoinTokenPrefix) {
		t.Fatalf("prefix: %q", s)
	}
	got, err := ParseJoinToken(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.HubURL != tok.HubURL || got.ID != tok.ID || !bytes.Equal(got.Secret, tok.Secret) || got.CAFingerprint != tok.CAFingerprint {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, tok)
	}
	if got.SecretHash() != tok.SecretHash() || got.SecretHash() != HashSecret(tok.Secret) || !isLowerHex(tok.SecretHash(), 64) {
		t.Fatal("secret hash mismatch")
	}
	if !ConstantTimeEqualHex(got.SecretHash(), tok.SecretHash()) {
		t.Fatal("ConstantTimeEqualHex false for equal hashes")
	}
	other, err := NewJoinToken("https://hub.example.com:7777", ca.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == tok.ID || bytes.Equal(other.Secret, tok.Secret) {
		t.Fatal("tokens not random")
	}
	if ConstantTimeEqualHex(other.SecretHash(), tok.SecretHash()) {
		t.Fatal("different secrets compare equal")
	}
}

func TestHashSecretKnownValue(t *testing.T) {
	// sha256("abc")
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashSecret([]byte("abc")); got != want {
		t.Fatalf("HashSecret = %q", got)
	}
}

func TestConstantTimeEqualHex(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"abcd", "abcd", true},
		{"abcd", "abce", false},
		{"abcd", "abc", false},
		{"", "", false},
	}
	for _, tc := range tests {
		if got := ConstantTimeEqualHex(tc.a, tc.b); got != tc.want {
			t.Errorf("(%q,%q) = %v", tc.a, tc.b, got)
		}
	}
}

func encodePayload(t *testing.T, p *mycelv1.JoinTokenPayload) string {
	t.Helper()
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return JoinTokenPrefix + b64.EncodeToString(raw)
}

func TestParseJoinTokenRejects(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	id := strings.Repeat("0f", 16)
	secret := bytes.Repeat([]byte{7}, 32)
	valid := func() *mycelv1.JoinTokenPayload {
		return &mycelv1.JoinTokenPayload{HubUrl: "https://hub:7777", TokenId: id, Secret: secret, CaFingerprint: fp}
	}
	with := func(f func(p *mycelv1.JoinTokenPayload)) string {
		p := valid()
		f(p)
		return encodePayload(t, p)
	}
	good := encodePayload(t, valid())
	if _, err := ParseJoinToken(good); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	body := strings.TrimPrefix(good, JoinTokenPrefix)
	tampered := []byte(good)
	tampered[len(JoinTokenPrefix)+3] ^= 0x01

	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"garbage", "hello world"},
		{"wrong prefix", "mcl2_" + body},
		{"agent prefix", AgentTokenPrefix + body},
		{"no prefix", body},
		{"bad base64", JoinTokenPrefix + "!!!!"},
		{"padded base64", JoinTokenPrefix + body + "=="},
		{"not proto", JoinTokenPrefix + b64.EncodeToString([]byte{0xff, 0xff, 0xff})},
		{"tampered", string(tampered)},
		{"truncated", good[:len(good)-10]},
		{"short id", with(func(p *mycelv1.JoinTokenPayload) { p.TokenId = "abcd" })},
		{"uppercase id", with(func(p *mycelv1.JoinTokenPayload) { p.TokenId = strings.ToUpper(id) })},
		{"non-hex id", with(func(p *mycelv1.JoinTokenPayload) { p.TokenId = strings.Repeat("zz", 16) })},
		{"short secret", with(func(p *mycelv1.JoinTokenPayload) { p.Secret = secret[:31] })},
		{"long secret", with(func(p *mycelv1.JoinTokenPayload) { p.Secret = append(bytes.Clone(secret), 1) })},
		{"short fingerprint", with(func(p *mycelv1.JoinTokenPayload) { p.CaFingerprint = fp[:62] })},
		{"uppercase fingerprint", with(func(p *mycelv1.JoinTokenPayload) { p.CaFingerprint = strings.ToUpper(fp) })},
		{"colon fingerprint", with(func(p *mycelv1.JoinTokenPayload) { p.CaFingerprint = "ab:" + fp[3:] })},
		{"ftp url", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "ftp://hub" })},
		{"no scheme", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "hub:7777" })},
		{"http url", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "http://hub:7777" })},
		{"userinfo", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://user@hub:7777" })},
		{"query", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://hub:7777/?a=b" })},
		{"empty query", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://hub:7777/?" })},
		{"fragment", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://hub:7777/#x" })},
		{"embedded newline", good[:10] + "\n" + good[10:]},
		{"embedded CR", good[:10] + "\r" + good[10:]},
		{"trailing newline", good + "\n"},
		{"no host", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://" })},
		{"port only", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "https://:7777" })},
		{"empty url", with(func(p *mycelv1.JoinTokenPayload) { p.HubUrl = "" })},
		{"too long", JoinTokenPrefix + strings.Repeat("A", maxJoinTokenLen)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseJoinToken(tc.in); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

func TestNewJoinTokenValidates(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	tests := []struct {
		url, fp string
		ok      bool
	}{
		{"https://hub:7777", fp, true},
		{"https://192.168.1.2:7777", fp, true},
		{"http://192.168.1.2:7777", fp, false},
		{"https://user:pw@hub", fp, false},
		{"https://hub?x=1", fp, false},
		{"https://hub#frag", fp, false},
		{"ftp://hub", fp, false},
		{"https://hub", "short", false},
	}
	for _, tc := range tests {
		_, err := NewJoinToken(tc.url, tc.fp)
		if tc.ok != (err == nil) {
			t.Errorf("NewJoinToken(%q,%q) err=%v", tc.url, tc.fp, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("want ErrInvalidArgument, got %v", err)
		}
	}
}

// ---- agent tokens ----

func mustSigner(t *testing.T) *AgentTokenSigner {
	t.Helper()
	key, err := NewAgentTokenKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("key len %d", len(key))
	}
	s, err := NewAgentTokenSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var testClaims = AgentClaims{
	AgentID:   "agent:trainer@gpu-box",
	NodeName:  "gpu-box",
	SandboxID: "sbx-123",
	OwnerID:   "user-1",
}

func TestAgentTokenRoundTrip(t *testing.T) {
	s := mustSigner(t)
	before := time.Now().UTC().Truncate(time.Second)
	tok, err := s.Issue(testClaims, AgentTokenLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, AgentTokenPrefix) || strings.Count(tok, ".") != 1 {
		t.Fatalf("format: %q", tok)
	}
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != testClaims.AgentID || got.NodeName != testClaims.NodeName ||
		got.SandboxID != testClaims.SandboxID || got.OwnerID != testClaims.OwnerID {
		t.Fatalf("claims mismatch: %+v", got)
	}
	if got.IssuedAt.Before(before) || got.IssuedAt.After(time.Now()) || got.IssuedAt.Location() != time.UTC {
		t.Fatalf("IssuedAt = %v", got.IssuedAt)
	}
	if got.IssuedAt.Nanosecond() != 0 {
		t.Fatal("IssuedAt not second precision")
	}
	if got.ExpiresAt.Sub(got.IssuedAt) != AgentTokenLifetime {
		t.Fatalf("ExpiresAt - IssuedAt = %v", got.ExpiresAt.Sub(got.IssuedAt))
	}

	// Trusted-host agents have no sandbox.
	c := testClaims
	c.SandboxID = ""
	tok2, err := s.Issue(c, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Verify(tok2); err != nil || got.SandboxID != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func replaceAt(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestAgentTokenRejects(t *testing.T) {
	s := mustSigner(t)
	other := mustSigner(t)
	tok, err := s.Issue(testClaims, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(tok, AgentTokenPrefix)
	payload, sig, _ := strings.Cut(body, ".")
	otherTok, err := other.Issue(testClaims, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// A forged payload with a different node, signed with the wrong key.
	forgedPayload := b64.EncodeToString([]byte(`{"aid":"agent:x@evil","node":"evil","iat":1,"exp":99999999999}`))
	expired, err := s.issueAt(testClaims, time.Hour, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Correctly signed tokens with bad claims.
	now := time.Now().Unix()
	signed := func(claimsJSON string) string {
		p := b64.EncodeToString([]byte(claimsJSON))
		return AgentTokenPrefix + p + "." + b64.EncodeToString(s.mac(p))
	}
	claimsJSON := func(aid, node string, iat, exp int64) string {
		return fmt.Sprintf(`{"aid":%q,"node":%q,"iat":%d,"exp":%d}`, aid, node, iat, exp)
	}
	badClaimsTok := signed(`{"aid":"agent:a@n","node":"n","iat":1,"exp":2,"extra":1}`)
	baseline := signed(claimsJSON("agent:a@n", "n", now, now+60))
	if _, err := s.Verify(baseline); err != nil {
		t.Fatalf("hand-built baseline rejected: %v", err)
	}
	// Signature over the payload alone (without prefix) must not verify.
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	noPrefixMAC := AgentTokenPrefix + payload + "." + b64.EncodeToString(m.Sum(nil))

	tests := []struct {
		name    string
		tok     string
		wantErr error
	}{
		{"tampered payload", AgentTokenPrefix + replaceAt(payload, 5) + "." + sig, ErrBadSignature},
		{"forged payload", AgentTokenPrefix + forgedPayload + "." + sig, ErrBadSignature},
		{"tampered signature", AgentTokenPrefix + payload + "." + replaceAt(sig, 0), ErrBadSignature},
		{"other key", otherTok, ErrBadSignature},
		{"expired", expired, ErrExpired},
		{"unknown claim field", badClaimsTok, ErrInvalidToken},
		{"empty", "", ErrInvalidToken},
		{"wrong prefix", "mca2_" + body, ErrInvalidToken},
		{"join prefix", JoinTokenPrefix + body, ErrInvalidToken},
		{"no dot", AgentTokenPrefix + payload + sig, ErrInvalidToken},
		{"two dots", tok + ".x", ErrInvalidToken},
		{"empty sig", AgentTokenPrefix + payload + ".", ErrInvalidToken},
		{"empty payload", AgentTokenPrefix + "." + sig, ErrInvalidToken},
		{"short sig", AgentTokenPrefix + payload + "." + sig[:10], ErrInvalidToken},
		{"garbage", "mca1_!!!.???", ErrInvalidToken},
		{"mac without prefix", noPrefixMAC, ErrBadSignature},
		{"100h window", signed(claimsJSON("agent:a@n", "n", now, now+100*3600)), ErrInvalidToken},
		{"zero window", signed(claimsJSON("agent:a@n", "n", now, now)), ErrInvalidToken},
		{"negative window", signed(claimsJSON("agent:a@n", "n", now, now-10)), ErrInvalidToken},
		{"issued in future", signed(claimsJSON("agent:a@n", "n", now+600, now+1200)), ErrInvalidToken},
		{"agent id other node", signed(claimsJSON("agent:a@m", "n", now, now+60)), ErrInvalidToken},
		{"agent id no prefix", signed(claimsJSON("a@n", "n", now, now+60)), ErrInvalidToken},
		{"agent id bad name", signed(claimsJSON("agent:A_b@n", "n", now, now+60)), ErrInvalidToken},
		{"agent id no at", signed(claimsJSON("agent:a", "n", now, now+60)), ErrInvalidToken},
		{"embedded newline in payload", AgentTokenPrefix + payload[:4] + "\n" + payload[4:] + "." + sig, ErrInvalidToken},
		{"embedded CR in sig", AgentTokenPrefix + payload + "." + sig[:4] + "\r" + sig[4:], ErrInvalidToken},
		{"trailing newline", tok + "\n", ErrInvalidToken},
		{"padding", tok + "=", ErrInvalidToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Verify(tc.tok); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}

	// Verify at a time past expiry.
	if _, err := s.verifyAt(tok, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestAgentTokenIssueRejects(t *testing.T) {
	s := mustSigner(t)
	noAgent := testClaims
	noAgent.AgentID = ""
	badNode := testClaims
	badNode.NodeName = "Bad Node"
	mismatch := testClaims
	mismatch.AgentID = "agent:trainer@other-box"
	badAgentName := testClaims
	badAgentName.AgentID = "agent:Trainer@gpu-box"
	tests := []struct {
		name    string
		claims  AgentClaims
		ttl     time.Duration
		wantErr error
	}{
		{"zero ttl", testClaims, 0, ErrInvalidArgument},
		{"negative ttl", testClaims, -time.Minute, ErrInvalidArgument},
		{"no agent id", noAgent, time.Hour, ErrInvalidArgument},
		{"bad node", badNode, time.Hour, ErrInvalidName},
		{"sub-second ttl", testClaims, 500 * time.Millisecond, ErrInvalidArgument},
		{"ttl 100h", testClaims, 100 * time.Hour, ErrInvalidArgument},
		{"ttl just above lifetime", testClaims, AgentTokenLifetime + time.Second, ErrInvalidArgument},
		{"agent id other node", mismatch, time.Hour, ErrInvalidArgument},
		{"agent id bad name", badAgentName, time.Hour, ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Issue(tc.claims, tc.ttl); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestNewAgentTokenSignerKeyLength(t *testing.T) {
	tests := []struct {
		n  int
		ok bool
	}{
		{0, false},
		{16, false},
		{31, false},
		{32, true},
		{64, true},
	}
	for _, tc := range tests {
		key := make([]byte, tc.n)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		_, err := NewAgentTokenSigner(key)
		if tc.ok != (err == nil) {
			t.Errorf("len %d: err=%v", tc.n, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidKey) {
			t.Errorf("len %d: want ErrInvalidKey, got %v", tc.n, err)
		}
	}
}

func TestAgentTokenSignerCopiesKey(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	s, err := NewAgentTokenSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Issue(testClaims, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key[0] = 2
	if _, err := s.Verify(tok); err != nil {
		t.Fatalf("mutating caller key affected signer: %v", err)
	}
}
