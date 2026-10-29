// Package fakemdns replaces mDNS in the test build of mycel (build tag
// fakeharness, see cmd/mycel/discovery_fake.go) with a shared directory:
// a hub "advertises" by writing its record as a JSON file into
// $MYCEL_FAKE_MDNS_DIR (removed when advertising stops) and a browser
// lists the records found there. Several processes on one machine (a hub,
// a joining node, `mycel hub discover`) see each other's records the way
// they would on a LAN, and nothing reaches the network.
//
// Without MYCEL_FAKE_MDNS_DIR a test binary neither advertises nor
// browses (both fail with ErrOff), so a test that forgets --no-mdns cannot
// announce a hub on the developer's network. MYCEL_TEST_MDNS=1 keeps the
// real mDNS for the opt-in multicast tests (Linux CI).
package fakemdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richard-sen27/mycelium/internal/discovery"
)

// EnvDir names the directory shared by the processes of one test.
const EnvDir = "MYCEL_FAKE_MDNS_DIR"

// EnvReal keeps real mDNS in a test binary.
const EnvReal = "MYCEL_TEST_MDNS"

// ErrOff is returned when the test binary runs without EnvDir.
var ErrOff = errors.New("mDNS is off in this test build (set " + EnvDir + ", or " + EnvReal + "=1 for real multicast)")

func init() {
	if os.Getenv(EnvReal) == "1" {
		return
	}
	discovery.SetOverride(&discovery.Override{
		Advertise: advertise,
		Browser:   func() discovery.Browser { return browser{dir: os.Getenv(EnvDir)} },
	})
}

// record is one advertised hub on disk.
type record struct {
	TrustDomain   string `json:"td"`
	URL           string `json:"url"`
	BusURL        string `json:"bus"`
	CAFingerprint string `json:"fp"`
	PID           int    `json:"pid"`
}

var seq atomic.Int64

type advertiser struct {
	path string
	once sync.Once
}

func (a *advertiser) Close() error {
	a.once.Do(func() { _ = os.Remove(a.path) })
	return nil
}

func advertise(ctx context.Context, h discovery.Hub, _ discovery.AdvertiseOptions) (discovery.Advertiser, error) {
	dir := os.Getenv(EnvDir)
	if dir == "" {
		return nil, ErrOff
	}
	fp := strings.ToLower(h.CAFingerprint)
	if h.TrustDomain == "" || len(fp) != 64 || strings.Trim(fp, "0123456789abcdef") != "" {
		return nil, errors.New("fakemdns: trust domain and CA fingerprint (64 hex characters) are required")
	}
	b, err := json.Marshal(record{TrustDomain: h.TrustDomain, URL: h.URL, BusURL: h.BusURL, CAFingerprint: fp, PID: os.Getpid()})
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%d-%d.json", fp[:16], os.Getpid(), seq.Add(1))
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:gosec // the test's shared directory
		return nil, fmt.Errorf("fakemdns: %w", err)
	}
	a := &advertiser{path: filepath.Join(dir, name)}
	if err := os.Rename(tmp, a.path); err != nil { //nolint:gosec // the test's shared directory
		_ = os.Remove(tmp) //nolint:gosec // the test's shared directory
		return nil, fmt.Errorf("fakemdns: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = a.Close()
	}()
	return a, nil
}

type browser struct{ dir string }

// Browse returns the records in the directory at once (there is nothing
// to wait for).
func (b browser) Browse(ctx context.Context, _ time.Duration) ([]discovery.Hub, error) {
	if b.dir == "" {
		return nil, ErrOff
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(b.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []discovery.Hub
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // the test's shared directory
		if err != nil {
			continue // removed meanwhile
		}
		var r record
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		h := discovery.Hub{TrustDomain: r.TrustDomain, URL: r.URL, BusURL: r.BusURL, CAFingerprint: r.CAFingerprint,
			Addresses: []string{}}
		if u, err := url.Parse(r.URL); err == nil {
			h.Addresses = append(h.Addresses, u.Hostname())
			h.Port, _ = strconv.Atoi(u.Port())
		}
		out = append(out, h)
	}
	return out, nil
}
