package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/richard-sen27/mycelium/internal/identity"
)

// URLScheme is the scheme of a relay URL.
const URLScheme = "relay"

// DefaultControlPort is the relay's default control port.
const DefaultControlPort = 7790

// tokenBytes is the size of a relay token secret.
const tokenBytes = 32

// RelayURL is a parsed relay URL:
//
//	relay://<host>:<control port>/<hub name>?fp=<sha256 hex>&token=<base64url secret>
type RelayURL struct {
	Host        string // public host of the relay (DNS name or IP)
	Port        int    // control port
	Name        string // hub name the token belongs to
	Fingerprint string // lowercase hex SHA-256 of the relay certificate (DER)
	Token       string // base64url (no padding) secret; never logged
}

// ParseRelayURL parses and validates a relay URL.
func ParseRelayURL(s string) (*RelayURL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("tunnel: invalid relay URL")
	}
	if u.Scheme != URLScheme {
		return nil, fmt.Errorf("tunnel: relay URL must start with %s://", URLScheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("tunnel: relay URL has no host")
	}
	port := DefaultControlPort
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("tunnel: relay URL has an invalid port")
		}
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !ValidHubName(name) {
		return nil, errors.New("tunnel: relay URL needs a hub name path (lowercase letters, digits, '-')")
	}
	q := u.Query()
	fp := strings.ToLower(q.Get("fp"))
	if !validFingerprint(fp) {
		return nil, errors.New("tunnel: relay URL needs fp=<64 hex characters>")
	}
	tok := q.Get("token")
	if b, err := base64.RawURLEncoding.DecodeString(tok); err != nil || len(b) != tokenBytes {
		return nil, errors.New("tunnel: relay URL needs token=<base64url secret>")
	}
	return &RelayURL{Host: host, Port: port, Name: name, Fingerprint: fp, Token: tok}, nil
}

// String formats the URL including the secret.
func (u *RelayURL) String() string {
	q := url.Values{}
	q.Set("fp", u.Fingerprint)
	q.Set("token", u.Token)
	return (&url.URL{Scheme: URLScheme, Host: u.Addr(), Path: "/" + u.Name, RawQuery: q.Encode()}).String()
}

// Redacted formats the URL without the secret, for logs and status output.
func (u *RelayURL) Redacted() string {
	return URLScheme + "://" + u.Addr() + "/" + u.Name
}

// Addr is the control address host:port.
func (u *RelayURL) Addr() string { return net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) }

// ValidHubName reports whether name is acceptable as a relay token name (the
// node-name rule: 1-63 of [a-z0-9-], no leading or trailing '-').
func ValidHubName(name string) bool { return identity.ValidNodeName(name) }

func validFingerprint(fp string) bool {
	if len(fp) != 64 {
		return false
	}
	_, err := hex.DecodeString(fp)
	return err == nil
}

// newSecret returns a fresh base64url token secret.
func newSecret() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tunnel: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
