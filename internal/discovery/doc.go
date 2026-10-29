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
// `fp` equals the token's CA fingerprint, trying each such hub in turn (an
// impostor can copy the fingerprint but not pass the pinned-CA TLS
// handshake that follows). `mycel hub discover` lists every hub seen. A node that
// cannot reach its hub for 60 s also browses, and follows a hub with its
// pinned CA fingerprint (this is how nodes find a hub laptop whose address
// changed on the same network).
//
// Library: github.com/brutella/dnssd (pure Go on macOS, Linux and
// Windows; see docs/decisions.md D026). Tests inject a fake browser and
// never touch the network unless MYCEL_TEST_MDNS=1 (browsing everywhere,
// advertising on Linux only). The test build of the binary (build tag
// fakeharness) replaces mDNS with a directory shared by the processes of
// one test (internal/discovery/fakemdns, SetOverride).
package discovery

import (
	"context"
	"log/slog"
	"time"
)

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

// AdvertiseOptions configures Advertise.
type AdvertiseOptions struct {
	// PIDFile records the pid of the system responder child (the hub uses
	// <hub dir>/mdns.pid); a responder named there by a hub that died is
	// stopped first. Empty: no pid file.
	PIDFile string
	Logger  *slog.Logger
}

// Advertise starts announcing h; it stops when ctx ends or Close is called.
func Advertise(ctx context.Context, h Hub, opts AdvertiseOptions) (Advertiser, error) {
	if o := loadOverride(); o != nil {
		return o.Advertise(ctx, h, opts)
	}
	return advertise(ctx, h, opts)
}

// Browser finds hubs. The default browser uses mDNS; tests inject fakes.
type Browser interface {
	// Browse collects hubs seen within timeout.
	Browse(ctx context.Context, timeout time.Duration) ([]Hub, error)
}

// Default returns the mDNS browser.
func Default() Browser {
	if o := loadOverride(); o != nil {
		return o.Browser()
	}
	return mdnsBrowser{}
}

// Find returns every hub with the given CA fingerprint seen within timeout
// (an impostor can copy a fingerprint, so callers probe each in turn over
// TLS verified against the pinned CA), or ErrNotFound.
func Find(ctx context.Context, b Browser, caFingerprint string, timeout time.Duration) ([]Hub, error) {
	return find(ctx, b, caFingerprint, timeout)
}
