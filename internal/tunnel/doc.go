// Package tunnel exposes a hub that sits behind NAT or a venue network
// through a relay running on any machine with a public address (spec "Hub
// placement and reachability": "a small relay on any VPS"). Phase 7
// contract, owned by P7-relay.
//
// Shape. The relay is the same binary (`mycel relay serve`), one hub per
// relay. It owns two kinds of listeners:
//
//   - Public TCP listeners, one per forwarded port (default 443 for the hub
//     API and 7778 for the bus). Every accepted connection is forwarded
//     byte-for-byte to the hub over the tunnel. The relay never terminates
//     TLS: the hub presents its own certificate (CA-issued, or ACME: the
//     TLS-ALPN-01 challenge passes through unchanged), nodes keep doing mTLS
//     with the hub, and the relay sees ciphertext only.
//   - A control listener (default :7790) with TLS and a self-signed
//     certificate generated at first start. The hub dials it, pins the
//     certificate fingerprint from the relay URL, authenticates with the
//     relay token, and keeps one multiplexed session (hashicorp/yamux)
//     open; each public connection becomes one stream. The hub reconnects
//     with exponential backoff (1 s to 30 s). While no hub session is
//     attached the public listeners refuse connections.
//
// Relay URL and token. `mycel relay token --name <hub name>` creates a
// token on the relay (32 random bytes; the relay stores only its SHA-256)
// and prints the URL the hub uses:
//
//	relay://<host>:<control port>/<hub name>?fp=<sha256 of the relay certificate>&token=<base64url secret>
//
// The URL carries a secret, so the hub stores it in <hub dir>/relay.url
// (0600), never in hub.yaml, logs or audit. `mycel relay token revoke`
// removes it.
//
// Hub side. `mycel hub init --tunnel <relay url>` (or `mycel hub tunnel
// set <url>` later) stores the URL, sets `tunnel.enabled: true` and the
// forwarded ports in hub.yaml, and makes the advertise URL and bus
// advertise point at the relay's public host. On every start the hub
// Dials the relay and serves its API and bus listeners over the tunnel
// streams in addition to (not instead of) its local listeners.
//
// Limits and hygiene: at most 4096 forwarded connections per hub, 64 new
// connections per second per source address, 10 minutes idle timeout per
// forwarded connection, 30 s handshake timeout on the control listener,
// a 32 KiB read buffer per stream, `tcp_nodelay` on both ends. The relay
// logs connection counts, never payloads. Relay state (certificate, token
// hashes) lives in `--data-dir` (default ~/.mycel/relay), files 0600.
//
// Also in this package: the hub's ACME support (`acme:` in hub.yaml):
// golang.org/x/crypto/acme/autocert with TLS-ALPN-01 for the advertise
// host, cache in <hub dir>/acme, the email from hub.yaml; the API and bus
// listeners use the ACME certificate for the advertise host and the
// CA-issued certificate for every other name. Nodes and CLIs accept a
// hub server certificate chained to either the pinned hub CA or the
// system roots (the client certificate is what identifies a node).
package tunnel

import "errors"

// ErrNotImplemented marks contract stubs.
var ErrNotImplemented = errors.New("tunnel: not implemented")

// Config is the hub.yaml `tunnel:` section.
type Config struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// Ports maps public relay ports to the hub's role: "api" or "bus".
	// Default {443: api, 7778: bus}.
	Ports map[int]string `yaml:"ports,omitempty"`
}

// ACMEConfig is the hub.yaml `acme:` section.
type ACMEConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	Email   string `yaml:"email,omitempty"`
	// Directory overrides the ACME directory URL (tests use a local CA
	// such as pebble); empty means Let's Encrypt production.
	Directory string `yaml:"directory,omitempty"`
}

// RelayURLFile is the file under the hub directory that holds the relay
// URL with its secret.
const RelayURLFile = "relay.url"
