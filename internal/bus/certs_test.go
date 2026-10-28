package bus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

// testCA is a throwaway CA built directly with crypto/x509 (the identity
// package is not used here on purpose).
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T, td string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(t),
		Subject:               pkix.Name{CommonName: "test CA " + td},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: td}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func randSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (ca *testCA) issue(t *testing.T, tmpl *x509.Certificate) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = randSerial(t)
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, cert
}

// serverCert issues the hub's bus server certificate for 127.0.0.1/localhost.
func (ca *testCA) serverCert(t *testing.T) tls.Certificate {
	t.Helper()
	c, _ := ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "hub"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	})
	return c
}

// clientCert issues a client certificate with the given URI SANs (none if
// uris is empty).
func (ca *testCA) clientCert(t *testing.T, cn string, uris ...string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	for _, s := range uris {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	return ca.issue(t, tmpl)
}

// shortNodeCert issues a node certificate that expires at notAfter.
func (ca *testCA) shortNodeCert(t *testing.T, td, name string, notAfter time.Time) tls.Certificate {
	t.Helper()
	u, err := url.Parse("spiffe://" + td + "/node/" + name)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{u},
		NotAfter:    notAfter,
	})
	return c
}

// nodeCert issues a node certificate spiffe://<td>/node/<name>.
func (ca *testCA) nodeCert(t *testing.T, td, name string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	return ca.clientCert(t, name, "spiffe://"+td+"/node/"+name)
}
