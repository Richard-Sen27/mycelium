package tunnel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Files in the relay data directory (all 0600).
const (
	relayCertFile   = "relay.crt"
	relayKeyFile    = "relay.key"
	relayTokensFile = "tokens.json"
)

// relayCertLifetime is the validity of the self-signed relay certificate.
const relayCertLifetime = 10 * 365 * 24 * time.Hour

// TokenInfo is one token in tokens.json: the hub name, the SHA-256 of the
// secret (hex) and when it was created. The secret itself is never stored.
type TokenInfo struct {
	Name      string `json:"name"`
	Hash      string `json:"sha256"`
	CreatedAt string `json:"created_at"`
}

type tokenFile struct {
	Tokens []TokenInfo `json:"tokens"`
}

func ensureDataDir(dir string) error {
	if dir == "" {
		return errors.New("tunnel: relay data directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(dir, 0o700) //nolint:gosec // a directory needs the x bit
	}
	return nil
}

// LoadOrCreateRelayCert returns the relay's TLS certificate from dataDir,
// creating a self-signed ECDSA P-256 certificate valid for ten years on
// first use, and its fingerprint (lowercase hex SHA-256 of the DER).
func LoadOrCreateRelayCert(dataDir string) (tls.Certificate, string, error) {
	if err := ensureDataDir(dataDir); err != nil {
		return tls.Certificate{}, "", err
	}
	certPath, keyPath := filepath.Join(dataDir, relayCertFile), filepath.Join(dataDir, relayKeyFile)
	certPEM, cerr := os.ReadFile(certPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	switch {
	case cerr == nil && kerr == nil:
	case errors.Is(cerr, fs.ErrNotExist) && errors.Is(kerr, fs.ErrNotExist):
		var err error
		certPEM, keyPEM, err = newRelayCert()
		if err != nil {
			return tls.Certificate{}, "", err
		}
		if err := writeExclusive(keyPath, keyPEM); err != nil {
			return tls.Certificate{}, "", err
		}
		if err := writeExclusive(certPath, certPEM); err != nil {
			return tls.Certificate{}, "", err
		}
	default:
		return tls.Certificate{}, "", fmt.Errorf("tunnel: relay certificate in %s is incomplete (%s and %s must both exist)", dataDir, relayCertFile, relayKeyFile)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("tunnel: relay certificate: %w", err)
	}
	return cert, certFingerprint(cert.Certificate[0]), nil
}

func newRelayCert() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mycel relay"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(relayCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ErrTokenExists is returned by CreateToken when the relay already has a
// token and replace is false.
var ErrTokenExists = errors.New("tunnel: this relay already has a hub token (one hub per relay)")

// CreateToken creates the token for hub name in the relay's tokens.json
// and returns the secret (shown once). A relay serves one hub: when any
// token exists CreateToken fails with ErrTokenExists unless replace is
// set, which revokes every existing token (a running relay drops their
// session within a few seconds).
func CreateToken(dataDir, name string, replace bool) (string, error) {
	if !ValidHubName(name) {
		return "", fmt.Errorf("tunnel: invalid hub name %q (lowercase letters, digits, '-')", name)
	}
	if err := ensureDataDir(dataDir); err != nil {
		return "", err
	}
	tf, err := readTokens(dataDir)
	if err != nil {
		return "", err
	}
	if len(tf.Tokens) > 0 {
		if !replace {
			names := make([]string, 0, len(tf.Tokens))
			for _, t := range tf.Tokens {
				names = append(names, t.Name)
			}
			return "", fmt.Errorf("%w: %s", ErrTokenExists, strings.Join(names, ", "))
		}
		tf.Tokens = nil
	}
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	tf.Tokens = append(tf.Tokens, TokenInfo{Name: name, Hash: hashSecret(secret), CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := writeTokens(dataDir, tf); err != nil {
		return "", err
	}
	return secret, nil
}

// ListTokens returns the relay's tokens (names and hashes) sorted by name.
func ListTokens(dataDir string) ([]TokenInfo, error) {
	tf, err := readTokens(dataDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(tf.Tokens, func(i, j int) bool { return tf.Tokens[i].Name < tf.Tokens[j].Name })
	return tf.Tokens, nil
}

// RevokeToken removes the token of hub name. A running relay drops that
// hub's session within a few seconds.
func RevokeToken(dataDir, name string) error {
	tf, err := readTokens(dataDir)
	if err != nil {
		return err
	}
	out := tf.Tokens[:0]
	found := false
	for _, t := range tf.Tokens {
		if t.Name == name {
			found = true
			continue
		}
		out = append(out, t)
	}
	if !found {
		return fmt.Errorf("tunnel: no token for %q", name)
	}
	tf.Tokens = out
	return writeTokens(dataDir, tf)
}

func readTokens(dataDir string) (*tokenFile, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, relayTokensFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &tokenFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("tunnel: parse %s: %w", relayTokensFile, err)
	}
	return &tf, nil
}

func writeTokens(dataDir string, tf *tokenFile) error {
	if tf.Tokens == nil {
		tf.Tokens = []TokenInfo{}
	}
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dataDir, relayTokensFile), append(b, '\n'))
}

// writeExclusive creates path with mode 0600; it never writes through an
// existing file or link.
func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// WriteFileAtomic writes data to path with mode 0600 through a temporary
// file in the same directory and a rename.
func WriteFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	// CreateTemp creates the file with mode 0600.
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
