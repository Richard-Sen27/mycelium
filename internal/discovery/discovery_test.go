package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brutella/dnssd"
)

var (
	fpA = strings.Repeat("a", 64)
	fpB = strings.Repeat("b", 64)
)

type fakeBrowser struct {
	hubs []Hub
	err  error
}

func (f fakeBrowser) Browse(context.Context, time.Duration) ([]Hub, error) { return f.hubs, f.err }

func TestFindMatchesFingerprintOnly(t *testing.T) {
	b := fakeBrowser{hubs: []Hub{
		{TrustDomain: "team", URL: "https://impostor:7777", CAFingerprint: fpB},
		{TrustDomain: "team", URL: "https://real:7777", CAFingerprint: fpA},
	}}
	b.hubs = append(b.hubs, Hub{TrustDomain: "team", URL: "https://copycat:7777", CAFingerprint: fpA})
	hs, err := Find(context.Background(), b, strings.ToUpper(fpA), time.Second)
	if err != nil || len(hs) != 2 || hs[0].URL != "https://real:7777" || hs[1].URL != "https://copycat:7777" {
		t.Fatalf("Find = %+v, %v", hs, err)
	}
	if _, err := Find(context.Background(), b, strings.Repeat("c", 64), time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no match: %v", err)
	}
	if _, err := Find(context.Background(), b, "short", time.Second); err == nil {
		t.Fatal("invalid fingerprint accepted")
	}
	berr := errors.New("boom")
	if _, err := Find(context.Background(), fakeBrowser{err: berr}, fpA, time.Second); !errors.Is(err, berr) {
		t.Fatalf("browse error: %v", err)
	}
	// A match is returned even when the browse also reported an error.
	if hs, err := Find(context.Background(), fakeBrowser{hubs: b.hubs, err: berr}, fpA, time.Second); err != nil || len(hs) != 2 {
		t.Fatalf("partial: %+v %v", hs, err)
	}
}

func TestHubFromTXT(t *testing.T) {
	good := map[string]string{"td": "team", "url": "https://h:7777", "bus": "tls://h:7778", "fp": strings.ToUpper(fpA), "v": "1"}
	h, ok := hubFromTXT("inst", good, 7777, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::1")})
	if !ok || h.TrustDomain != "team" || h.CAFingerprint != fpA || h.BusURL != "tls://h:7778" || len(h.Addresses) != 2 || h.Port != 7777 {
		t.Fatalf("hub %+v ok=%v", h, ok)
	}
	for _, mut := range []func(m map[string]string){
		func(m map[string]string) { m["v"] = "2" },
		func(m map[string]string) { m["url"] = "http://h:7777" },
		func(m map[string]string) { m["fp"] = "zz" },
		func(m map[string]string) { delete(m, "url") },
	} {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		mut(m)
		if _, ok := hubFromTXT("inst", m, 1, nil); ok {
			t.Errorf("invalid record accepted: %v", m)
		}
	}
	m := map[string]string{"url": "https://h", "fp": fpA, "v": "1"}
	if h, ok := hubFromTXT("inst", m, 1, nil); !ok || h.TrustDomain != "inst" {
		t.Fatalf("instance name fallback: %+v %v", h, ok)
	}
}

func TestTXTValidation(t *testing.T) {
	ok := Hub{TrustDomain: "team", URL: "https://h:7777", BusURL: "tls://h:7778", CAFingerprint: fpA}
	txt, err := txtFor(ok)
	if err != nil || txt["v"] != "1" || txt["fp"] != fpA || txt["td"] != "team" || txt["url"] != ok.URL || txt["bus"] != ok.BusURL {
		t.Fatalf("txt %v, %v", txt, err)
	}
	for _, bad := range []Hub{
		{URL: "https://h", CAFingerprint: fpA},
		{TrustDomain: "t", URL: "http://h", CAFingerprint: fpA},
		{TrustDomain: "t", URL: "https://h", CAFingerprint: "x"},
		{TrustDomain: "t", URL: "https://h/" + strings.Repeat("p", 300), CAFingerprint: fpA},
	} {
		if _, err := txtFor(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
		if _, err := Advertise(context.Background(), bad, AdvertiseOptions{}); err == nil {
			t.Errorf("Advertise accepted %+v", bad)
		}
	}
}

func TestCollectorMergesAddresses(t *testing.T) {
	var c collector
	c.add(Hub{URL: "https://h", CAFingerprint: fpA, Addresses: []string{"192.0.2.1"}})
	c.add(Hub{URL: "https://h", CAFingerprint: fpA, Addresses: []string{"192.0.2.1", "fe80::1"}})
	c.add(Hub{URL: "https://other", CAFingerprint: fpA})
	l := c.list()
	if len(l) != 2 || len(l[0].Addresses) != 2 {
		t.Fatalf("list %+v", l)
	}
}

func TestUserlandResponderNeverClaimsHostname(t *testing.T) {
	txt, err := txtFor(Hub{TrustDomain: "team", URL: "https://h:7777", BusURL: "tls://h:7778", CAFingerprint: fpA})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := userlandService("team", 7777, txt, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(host, "."), ".local"))
	forbidden := []string{host + ".local.", strings.SplitN(host, ".", 2)[0] + ".local."}
	want := "mycel-hub-" + fpA[:8] + ".local."
	if got := srvTarget(srv); got != want {
		t.Fatalf("SRV target %q, want %q", got, want)
	}
	for _, n := range answeredNames(srv) {
		for _, f := range forbidden {
			if strings.EqualFold(n, f) {
				t.Fatalf("the responder would answer for the machine's host name %q", f)
			}
		}
	}
	if !slicesContainsFold(answeredNames(srv), want) {
		t.Fatalf("answered names %v lack the dedicated host %s", answeredNames(srv), want)
	}
}

func slicesContainsFold(l []string, s string) bool {
	for _, e := range l {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}

func TestAdvertiseCommandPerPlatform(t *testing.T) {
	defer func(g string, lp func(string) (string, error)) { goos, lookPath = g, lp }(goos, lookPath)
	txt := map[string]string{"td": "team", "url": "https://h:7777", "bus": "tls://h:7778", "fp": fpA, "v": "1"}
	found := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }

	goos, lookPath = "darwin", found
	p, args, err := advertiseCommand("team", 7777, txt)
	wantArgs := []string{"-R", "team", "_mycel-hub._tcp", "local", "7777", "td=team", "url=https://h:7777", "bus=tls://h:7778", "fp=" + fpA, "v=1"}
	if err != nil || p != "/usr/bin/dns-sd" || strings.Join(args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("darwin: %s %v %v", p, args, err)
	}
	goos, lookPath = "darwin", missing
	if _, _, err := advertiseCommand("team", 7777, txt); err == nil {
		t.Fatal("darwin without dns-sd must not fall back to the userland responder")
	}
	goos, lookPath = "linux", found
	p, args, err = advertiseCommand("team", 7777, txt)
	if err != nil || p != "/usr/bin/avahi-publish-service" || strings.Join(args, " ") != "-- team _mycel-hub._tcp 7777 "+strings.Join(wantArgs[5:], " ") {
		t.Fatalf("linux avahi: %s %v %v", p, args, err)
	}
	goos, lookPath = "linux", missing
	if p, _, err := advertiseCommand("team", 7777, txt); err != nil || p != "" {
		t.Fatalf("linux without avahi: %q %v", p, err)
	}
	goos, lookPath = "windows", found
	if p, _, err := advertiseCommand("team", 7777, txt); err != nil || p != "" {
		t.Fatalf("windows: %q %v", p, err)
	}
}

// TestBrowseOnly sends browse queries only (no announcements). Opt-in
// (MYCEL_TEST_MDNS=1): tests stay off the network by default.
func TestBrowseOnly(t *testing.T) {
	if os.Getenv("MYCEL_TEST_MDNS") != "1" {
		t.Skip("set MYCEL_TEST_MDNS=1 to send real mDNS queries")
	}
	start := time.Now()
	if _, err := Default().Browse(context.Background(), 300*time.Millisecond); err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Browse overran its timeout")
	}
}

// TestRealMulticast advertises a hub and finds it again over real mDNS.
// Opt-in (MYCEL_TEST_MDNS=1) and Linux CI only: on a workstation a
// responder bug could disturb the machine's own mDNS names (on macOS a
// userland responder that claimed the host name made mDNSResponder rename
// the computer).
func TestRealMulticast(t *testing.T) {
	if os.Getenv("MYCEL_TEST_MDNS") != "1" {
		t.Skip("set MYCEL_TEST_MDNS=1 to exercise real multicast (Linux CI only)")
	}
	if runtime.GOOS != "linux" {
		t.Skip("real multicast advertising runs on Linux CI runners only; on macOS it could rename the host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	td := fmt.Sprintf("mycel-test-%d", time.Now().UnixNano())
	fp := strings.Repeat("d", 64)
	want := Hub{TrustDomain: td, URL: "https://" + td + ".local:7777", BusURL: "tls://" + td + ".local:7778", CAFingerprint: fp, Port: 7777}
	adv, err := Advertise(ctx, want, AdvertiseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adv.Close() }()
	var got Hub
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var hs []Hub
		if hs, err = Find(ctx, Default(), fp, 3*time.Second); err == nil {
			got = hs[0]
			break
		}
	}
	if err != nil {
		t.Fatalf("hub not found over mDNS: %v", err)
	}
	if got.URL != want.URL || got.BusURL != want.BusURL || got.TrustDomain != td || got.Port != 7777 || len(got.Addresses) == 0 {
		t.Fatalf("found %+v", got)
	}
	t.Logf("found %s at %v", got.URL, got.Addresses)
	hubs, err := Default().Browse(ctx, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, h := range hubs {
		seen = seen || h.CAFingerprint == fp
	}
	if !seen {
		t.Fatalf("Browse did not list the hub: %+v", hubs)
	}
}

// srvTarget returns the SRV target the userland responder announces.
func srvTarget(srv dnssd.Service) string { return dnssd.SRV(srv).Target }
