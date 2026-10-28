//go:build darwin

package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/applevf"
)

// TestSandboxStatusOnThisMac runs `mycel sandbox status --json` against the
// real macOS backend's probes (limactl --version and limactl list in an
// empty LIMA_HOME; nothing is downloaded or started).
func TestSandboxStatusOnThisMac(t *testing.T) {
	// A short MYCEL_HOME: Lima limits its socket paths to 104 bytes.
	home, err := os.MkdirTemp("/tmp", "mycel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv(config.EnvHome, home)
	b := applevf.New()
	old := sandboxBackend
	sandboxBackend = func() sandbox.Backend { return b }
	t.Cleanup(func() { sandboxBackend = old })

	res := runCLI(t, "", "sandbox", "status", "--json")
	if res.code != 0 {
		t.Fatalf("%+v", res)
	}
	var got struct {
		Backend   string         `json:"backend"`
		Available bool           `json:"available"`
		Details   map[string]any `json:"details"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatalf("%v: %q", err, res.stdout)
	}
	if got.Backend != sandbox.BackendAppleVF || got.Details == nil {
		t.Fatalf("status %+v", got)
	}
	if got.Details["image"] != "docker.io/library/debian:bookworm-slim" {
		t.Errorf("image %v", got.Details["image"])
	}
	if got.Available {
		if got.Details["lima"] == nil || got.Details["vms"] != float64(0) {
			t.Errorf("available but details %v", got.Details)
		}
	} else if got.Details["reason"] == nil || got.Details["hint"] == nil {
		t.Errorf("unavailable without reason or hint: %v", got.Details)
	}
	t.Logf("status: %s", res.stdout)
}
