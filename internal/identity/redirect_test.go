package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

func caCert(t *testing.T, ca *CA) *x509.Certificate {
	t.Helper()
	c, err := ParseCertPEM(ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRedirectPayloadExact(t *testing.T) {
	got := string(RedirectPayload("my-team", "https://new:7777", "tls://new:7778", 1700000000))
	want := "mycel-redirect/1\nmy-team\nhttps://new:7777\ntls://new:7778\n1700000000\n"
	if got != want {
		t.Fatalf("payload %q, want %q", got, want)
	}
}

func TestRedirectECDSARoundTripAndTamper(t *testing.T) {
	ca := mustCA(t, testTD)
	cert := caCert(t, ca)
	now := time.Now().Unix()
	const hubURL, busURL = "https://new.example.com:7777", "tls://new.example.com:7778"
	sig, err := ca.SignRedirect(hubURL, busURL, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRedirect(cert, testTD, hubURL, busURL, now, sig, now); err != nil {
		t.Fatalf("verify: %v", err)
	}
	cases := []struct {
		name     string
		td       TrustDomain
		hub, bus string
		issued   int64
		sig      []byte
	}{
		{"trust domain", "other", hubURL, busURL, now, sig},
		{"hub url", testTD, "https://evil.example.com:7777", busURL, now, sig},
		{"bus url", testTD, hubURL, "tls://evil:7778", now, sig},
		{"issued at", testTD, hubURL, busURL, now - 1, sig},
		{"signature", testTD, hubURL, busURL, now, append([]byte{}, sig[:len(sig)-1]...)},
		{"empty signature", testTD, hubURL, busURL, now, nil},
	}
	for _, c := range cases {
		err := VerifyRedirect(cert, c.td, c.hub, c.bus, c.issued, c.sig, now)
		if err == nil {
			t.Errorf("%s: tampered redirect verified", c.name)
			continue
		}
		if !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err %v, want ErrBadSignature", c.name, err)
		}
	}
	// Another CA's signature never verifies.
	other := mustCA(t, testTD)
	osig, err := other.SignRedirect(hubURL, busURL, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRedirect(cert, testTD, hubURL, busURL, now, osig, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("foreign CA: %v", err)
	}
}

func TestRedirectWindow(t *testing.T) {
	ca := mustCA(t, testTD)
	cert := caCert(t, ca)
	now := time.Now().Unix()
	check := func(issued int64, ok bool) {
		t.Helper()
		sig, err := ca.SignRedirect("https://a:1", "tls://a:2", issued)
		if err != nil {
			t.Fatal(err)
		}
		err = VerifyRedirect(cert, testTD, "https://a:1", "tls://a:2", issued, sig, now)
		if ok && err != nil {
			t.Fatalf("issued %+d s: %v", issued-now, err)
		}
		if !ok && !errors.Is(err, ErrExpired) {
			t.Fatalf("issued %+d s: err %v, want ErrExpired", issued-now, err)
		}
	}
	check(now-RedirectMaxAge, true)
	check(now-RedirectMaxAge-1, false) // an old redirect
	check(now+RedirectMaxFuture, true)
	check(now+RedirectMaxFuture+1, false)
}

func TestRedirectEd25519(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ed"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	var signer crypto.Signer = priv
	sig, err := SignRedirect(signer, testTD, "https://b:1", "tls://b:2", now)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, RedirectPayload(testTD, "https://b:1", "tls://b:2", now), sig) {
		t.Fatal("ed25519 signs the message itself")
	}
	if err := VerifyRedirect(cert, testTD, "https://b:1", "tls://b:2", now, sig, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRedirect(cert, testTD, "https://b:1", "tls://c:2", now, sig, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
	// An ECDSA CA does not accept the Ed25519 signature.
	if err := VerifyRedirect(caCert(t, mustCA(t, testTD)), testTD, "https://b:1", "tls://b:2", now, sig, now); err == nil {
		t.Fatal("cross-scheme signature verified")
	}
}

func TestRedirectFieldHygiene(t *testing.T) {
	ca := mustCA(t, testTD)
	cert := caCert(t, ca)
	now := time.Now().Unix()
	bad := []struct{ hub, bus string }{
		{"https://a:1\nx", "tls://a:2"},   // newline in the hub URL
		{"https://a:1", "tls://a:2\x7f"},  // DEL
		{"https://a:1", "tls://a:\t2"},    // tab
		{"https://a:1", "tls://a:http"},   // non-numeric port
		{"https://a:1", "tls://a:0"},      // port 0
		{"https://a:1", "tls://a:65536"},  // out of range
		{"https://a:1", "tls://a:+2"},     // not canonical
		{"https://a:1", "tls://a"},        // no port
		{"https://a:1", "tls://a:2/path"}, // path
	}
	for _, b := range bad {
		if _, err := ca.SignRedirect(b.hub, b.bus, now); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("sign %q %q: %v", b.hub, b.bus, err)
		}
		if err := VerifyRedirect(cert, testTD, b.hub, b.bus, now, []byte{1}, now); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("verify %q %q: %v", b.hub, b.bus, err)
		}
	}
	for _, bus := range []string{"tls://a:2", "a:2", "tls://[::1]:7778"} {
		if _, err := ca.SignRedirect("https://a:1", bus, now); err != nil {
			t.Errorf("%q: %v", bus, err)
		}
	}
}
