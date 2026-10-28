//go:build !darwin && !linux && !windows

package inventory

import (
	"context"
	"runtime"
	"strings"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func probeGPUs(ctx context.Context) []*mycelv1.Gpu { return nvidiaGPUs(ctx) }

func probeBackends(context.Context) []string { return nil }

func formatOSVersion(platform, ver string) string {
	return strings.TrimSpace(platform + " " + ver)
}

func osVersionFallback(context.Context) string { return runtime.GOOS }
