//go:build darwin

package inventory

import (
	"context"
	"os"
	"os/exec"
	"strings"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const virtualizationFramework = "/System/Library/Frameworks/Virtualization.framework"

func probeGPUs(ctx context.Context) []*mycelv1.Gpu {
	gpus := nvidiaGPUs(ctx)
	out, err := runCmd(ctx, "/usr/sbin/system_profiler", "SPDisplaysDataType", "-json")
	if err != nil || len(out) == 0 {
		return gpus
	}
	more, err := parseSystemProfiler(out, int32(len(gpus))) //nolint:gosec // GPU count is tiny
	if err != nil {
		return gpus
	}
	return append(gpus, more...)
}

func probeBackends(_ context.Context) []string {
	var b []string
	if _, err := os.Stat(virtualizationFramework); err == nil {
		b = append(b, BackendAppleVF)
	}
	if _, err := exec.LookPath("container"); err == nil {
		b = append(b, BackendAppleContainer)
	}
	if _, err := exec.LookPath("limactl"); err == nil {
		b = append(b, BackendLima)
	}
	return b
}

// formatOSVersion turns gopsutil's ("darwin", "26.6.2") into "macOS 26.6.2".
func formatOSVersion(_, ver string) string {
	if ver == "" {
		return ""
	}
	return "macOS " + ver
}

func osVersionFallback(ctx context.Context) string {
	out, err := runCmd(ctx, "/usr/bin/sw_vers", "-productVersion")
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return "macOS " + v
	}
	return "macOS"
}
