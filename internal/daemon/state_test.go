package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	st := State{Name: "mac", SPIFFEID: "spiffe://td/node/mac", TrustDomain: "td", HubURL: "https://h:7777", BusURL: "tls://h:7778", OwnerID: "u1", Caps: []string{"docker"}}
	if HasState(dir) {
		t.Fatal("HasState before write")
	}
	if err := WriteState(dir, st, []byte("k"), []byte("c"), []byte("ca")); err != nil {
		t.Fatal(err)
	}
	if !HasState(dir) {
		t.Fatal("HasState after write")
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != st.Name || got.BusURL != st.BusURL || len(got.Caps) != 1 {
		t.Fatalf("got %+v", got)
	}
	if err := WriteState(dir, st, nil, nil, nil); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("second write: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, KeyFile))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %v", fi.Mode().Perm())
		}
	}
}

func TestReadStateMissing(t *testing.T) {
	if _, err := ReadState(t.TempDir()); !errors.Is(err, ErrNoNode) {
		t.Fatalf("got %v", err)
	}
}
