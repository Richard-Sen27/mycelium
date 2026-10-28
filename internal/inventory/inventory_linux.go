//go:build linux

package inventory

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const drmRoot = "/sys/class/drm"

// probeGPUs merges nvidia-smi results with AMD GPUs found in sysfs; AMD
// indexes continue after the NVIDIA ones.
func probeGPUs(ctx context.Context) []*mycelv1.Gpu {
	gpus := nvidiaGPUs(ctx)
	base := int32(len(gpus)) //nolint:gosec // GPU count is tiny
	for _, g := range amdSysfsGPUs(drmRoot) {
		g.Index += base
		gpus = append(gpus, g)
	}
	return gpus
}

func probeBackends(_ context.Context) []string {
	var b []string
	// Access, not Stat: a daemon that cannot open /dev/kvm cannot use it.
	if unix.Access("/dev/kvm", unix.R_OK|unix.W_OK) == nil {
		if _, err := exec.LookPath("firecracker"); err == nil {
			b = append(b, BackendFirecracker)
		}
	}
	if _, err := exec.LookPath("runsc"); err == nil {
		b = append(b, BackendGVisor)
	}
	return b
}

// formatOSVersion turns gopsutil's ("ubuntu", "24.04") into "Ubuntu 24.04".
func formatOSVersion(platform, ver string) string {
	if platform == "" {
		return ""
	}
	name := strings.ToUpper(platform[:1]) + platform[1:]
	return strings.TrimSpace(name + " " + ver)
}

// osVersionFallback reads NAME and VERSION_ID from /etc/os-release.
func osVersionFallback(_ context.Context) string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	defer func() { _ = f.Close() }()
	var name, ver string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "NAME":
			name = v
		case "VERSION_ID":
			ver = v
		}
	}
	if name == "" {
		return "Linux"
	}
	return strings.TrimSpace(name + " " + ver)
}
