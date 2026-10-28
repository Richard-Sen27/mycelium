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
// Relay URL and token. `mycel relay token create --name <hub name>`
// creates the relay's token (32 random bytes; the relay stores only its
// SHA-256) and prints the URL the hub uses:
//
//	relay://<host>:<control port>/<hub name>?fp=<sha256 of the relay certificate>&token=<base64url secret>
//
// One hub per relay: while a token exists, `token create` refuses unless
// `--replace` (which revokes the old token). The URL carries a secret, so
// it is printed on its own and the hub reads it from stdin (`-`); the hub
// stores it in <hub dir>/relay.url (0600), never in hub.yaml, logs or
// audit. `mycel relay token revoke` removes it.
//
// Hub side. `mycel hub init --tunnel -` (or `mycel hub tunnel set -`
// later) reads the URL from stdin, stores it, sets `tunnel.enabled: true`
// and the forwarded ports in hub.yaml, and makes the advertise URL and bus
// advertise point at the relay's public host. On every start the hub dials
// the relay and serves its API and bus listeners over the tunnel streams
// in addition to (not instead of) its local listeners. With `hub init
// --tunnel`, the hub machine's own node and CLI use the local listeners.
//
// Limits and hygiene: at most 1024 forwarded connections per hub (a 256 MiB
// budget of 256 KiB yamux stream windows, the smallest yamux allows) and
// 64 per source; 64 new connections per second per source; a source is an
// IPv4 address or an IPv6 /64. 10 minutes idle timeout per forwarded
// connection; on the control listener at most 64 connections waiting for
// their hello answer, 10 s to finish the TLS handshake and hello, and 4
// connections per second per source. A 32 KiB read buffer per stream,
// `tcp_nodelay` on both ends. The relay logs connection counts, never
// payloads. Relay state (certificate, token hashes) lives in `--data-dir`
// (default <data dir>/relay), files 0600.
//
// Trust. The relay is trusted for the source address it reports for each
// forwarded connection: the hub uses it for per-IP rate limits and records
// it in audit with `remote_via_relay: true`. Bus connections through the
// tunnel reach the bus from the hub's own loopback forwarder, so the bus
// sees a loopback source for them. The relay never sees plaintext and
// cannot impersonate the hub to nodes and CLIs (below).
//
// Also in this package: the hub's ACME support (`acme:` in hub.yaml):
// golang.org/x/crypto/acme/autocert with TLS-ALPN-01 for the advertise
// host, cache in <hub dir>/acme, the email from hub.yaml. The ACME
// certificate is for browsers only: the API and bus listeners present it
// for the advertise host unless the ClientHello lists the ALPN marker
// "mycel-ca" among its protocols, in which case they present the
// CA-issued certificate. Nodes and CLIs always send the marker and verify
// the hub against the pinned hub CA only; they never trust the system
// roots (a relay operator, or anyone with a public certificate for the
// hub's name, must not be able to impersonate the hub). The marker is
// never negotiated as the connection's protocol; h2/http/1.1 still win.
//
// Implementation notes (P7-relay, where the contract was silent):
//
//   - Control protocol: TLS 1.3 to the relay, pinned by VerifyConnection;
//     the hub sends one JSON line {"v":1,"name","token","ports"} and the
//     relay answers {"ok":true} or {"ok":false,"error":"..."} and closes
//     (an unknown name or wrong token is always "unauthorized"; a valid
//     hello for another hub name while a hub is attached gets "another hub
//     is attached to this relay"). The hub is the yamux client, the relay
//     the yamux server; the relay opens one stream per public connection
//     and never accepts hub-opened streams.
//   - Every stream starts with one JSON line {"port":443,"src":"ip:port"}
//     written by the relay: the hub maps the port to its role with its own
//     port map and uses src as the connection's RemoteAddr. The hub holds
//     at most 64 streams waiting for their header.
//   - A hello of the attached hub's own name replaces its session: the
//     relay opens one last stream on the old session with the header
//     {"close":"replaced"} and closes it; the hub logs that at error level
//     (two hubs sharing one relay URL) and counts it in its status.
//   - The hub resets its reconnect backoff only after a session that
//     lasted 30 s. Strings from the relay (refusal reasons) are stripped of
//     control characters and cut to 200 characters before they are logged,
//     stored or printed.
//   - The relay re-reads tokens.json every 5 s: a revoked token drops the
//     attached session. Its rate limiters hold at most 16384 sources and
//     forget the least recently seen one when full.
//   - ACME issuance never happens inside a client hello: a background
//     issuer starts once the hub serves (with a tunnel, after the first
//     attach), retries from 1 min doubling to 1 h, and after any failure
//     waits at least 10 min. Until it has a certificate, and after a
//     failure, browsers get the CA-issued certificate too. Only
//     TLS-ALPN-01 challenge hellos reach the ACME manager before that. Go
//     only asks for a per-hello certificate when the hello has SNI, so
//     connections by IP address always get the CA-issued certificate.
//   - `relay token create` takes --host (default: hostname, with a
//     warning) and --port (default 7790) for the printed URL.
//   - Status: GET /api/hub/tunnel on the hub (admins), read by
//     `mycel hub tunnel status`.
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
