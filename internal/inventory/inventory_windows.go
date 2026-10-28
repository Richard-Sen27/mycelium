//go:build windows

package inventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// wslSandboxDistro is the dedicated WSL2 distro that hosts gVisor sandboxes.
const wslSandboxDistro = "mycel-sbx"

func probeGPUs(ctx context.Context) []*mycelv1.Gpu {
	return nvidiaGPUs(ctx)
}

func probeBackends(ctx context.Context) []string {
	wsl := findWSL()
	if wsl == "" {
		return nil
	}
	// The inbox wsl.exe stub exists even when WSL is not enabled; --status
	// only exits 0 when it is.
	if _, err := runCmd(ctx, wsl, "--status"); err != nil {
		return nil
	}
	var b []string
	if out, err := runCmd(ctx, wsl, "-l", "-v"); err == nil && hasWSL2Distro(parseWSLVerbose(out), wslSandboxDistro) {
		b = append(b, BackendWSL2GVisor)
	}
	return append(b, BackendWSL2)
}

func findWSL() string {
	if p, err := exec.LookPath("wsl.exe"); err == nil {
		return p
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "wsl.exe")
		if _, err := os.Stat(p); err == nil { //nolint:gosec // fixed path under %SystemRoot%
			return p
		}
	}
	return ""
}

// formatOSVersion turns gopsutil's ("Microsoft Windows 11 Pro",
// "10.0.26100.4061 Build 26100.4061") into
// "Windows 11 Pro (10.0.26100.4061 Build 26100.4061)".
func formatOSVersion(platform, ver string) string {
	name := strings.TrimSpace(strings.TrimPrefix(platform, "Microsoft "))
	switch {
	case name == "" && ver == "":
		return ""
	case name == "":
		return "Windows " + ver
	case ver == "":
		return name
	}
	return name + " (" + ver + ")"
}

func osVersionFallback(_ context.Context) string {
	return "Windows"
}
