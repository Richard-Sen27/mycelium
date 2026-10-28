// Package discovery finds hubs on the local network with mDNS/DNS-SD (spec
// "Laptop on the same network: nodes find the hub by mDNS"). Phase 7
// contract, owned by P7-move.
//
// Service: `_mycel-hub._tcp.local.`, instance name = the trust domain,
// port = the API port, TXT records:
//
//	td=<trust domain>  url=<advertise url>  bus=<bus advertise>  fp=<CA fingerprint, lower hex>  v=1
//
// The hub advertises while `discovery.mdns` in hub.yaml is true (default
// true; `mycel hub init --no-mdns` turns it off). Advertising stops with
// the hub. Nothing secret is ever in a TXT record.
//
// Browsing: `mycel join <token>` first tries the hub URL inside the token;
// when that fails within 3 s it browses for 3 s and uses the hub whose
// `fp` equals the token's CA fingerprint (an impostor cannot satisfy the
// fingerprint; the URL it offers is then verified by the normal pinned-CA
// TLS handshake). `mycel hub discover` lists every hub seen. A node that
// cannot reach its hub for 60 s also browses, and follows a hub with its
// pinned CA fingerprint (this is how nodes find a hub laptop whose address
// changed on the same network).
//
// Library: github.com/grandcat/zeroconf or github.com/brutella/dnssd,
// whichever works on macOS, Linux and Windows without cgo; the choice and
// the reason go in docs/decisions.md D026. Tests inject a fake browser;
// an opt-in test (MYCEL_TEST_MDNS=1) exercises real multicast on loopback.
package discovery

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented marks contract stubs.
var ErrNotImplemented = errors.New("discovery: not implemented")

// ServiceType is the DNS-SD service type.
const ServiceType = "_mycel-hub._tcp"

// Hub is one advertised hub.
type Hub struct {
	TrustDomain   string
	URL           string // advertise URL (https://host:port)
	BusURL        string // bus advertise (host:port)
	CAFingerprint string // lower hex
	Addresses     []string
	Port          int
}

// Advertiser announces one hub while its context lives.
type Advertiser interface {
	Close() error
}

// Advertise starts announcing h; it stops when ctx ends or Close is called.
func Advertise(ctx context.Context, h Hub) (Advertiser, error) { panic(ErrNotImplemented) }

// Browser finds hubs. The default browser uses mDNS; tests inject fakes.
type Browser interface {
	// Browse collects hubs seen within timeout.
	Browse(ctx context.Context, timeout time.Duration) ([]Hub, error)
}

// Default returns the mDNS browser.
func Default() Browser { panic(ErrNotImplemented) }

// Find returns the first hub with the given CA fingerprint seen within
// timeout, or an error.
func Find(ctx context.Context, b Browser, caFingerprint string, timeout time.Duration) (Hub, error) {
	panic(ErrNotImplemented)
}
