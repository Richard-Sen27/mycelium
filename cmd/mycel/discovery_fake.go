//go:build fakeharness

package main

// The test build replaces mDNS with a shared directory (MYCEL_FAKE_MDNS_DIR)
// so multi-process tests never advertise on the network.
import _ "github.com/richard-sen27/mycelium/internal/discovery/fakemdns"
