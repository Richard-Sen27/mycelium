package fakemdns

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/discovery"
)

var testHub = discovery.Hub{
	TrustDomain: "td", URL: "https://127.0.0.1:7777", BusURL: "tls://127.0.0.1:7778",
	CAFingerprint: strings.Repeat("ab", 32),
}

func TestFakeMDNS(t *testing.T) {
	if os.Getenv(EnvReal) == "1" {
		t.Skip("real mDNS requested")
	}
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := discovery.Advertise(ctx, testHub, discovery.AdvertiseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hubs, err := discovery.Find(ctx, discovery.Default(), testHub.CAFingerprint, 0)
	if err != nil || len(hubs) != 1 || hubs[0].URL != testHub.URL || hubs[0].BusURL != testHub.BusURL || hubs[0].Port != 7777 {
		t.Fatalf("Find: %+v %v", hubs, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := discovery.Find(ctx, discovery.Default(), testHub.CAFingerprint, 0); !errors.Is(err, discovery.ErrNotFound) {
		t.Fatalf("Find after Close: %v", err)
	}

	// Without the directory, nothing is advertised or browsed.
	t.Setenv(EnvDir, "")
	if _, err := discovery.Advertise(ctx, testHub, discovery.AdvertiseOptions{}); !errors.Is(err, ErrOff) {
		t.Fatalf("Advertise without %s: %v", EnvDir, err)
	}
	if _, err := discovery.Default().Browse(ctx, 0); !errors.Is(err, ErrOff) {
		t.Fatalf("Browse without %s: %v", EnvDir, err)
	}
}
