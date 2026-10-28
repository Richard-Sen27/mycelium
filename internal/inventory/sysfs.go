package inventory

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Untagged so it is tested on every OS; only the Linux probe calls it.

var drmCardRe = regexp.MustCompile(`^card\d+$`)

// amdSysfsGPUs finds AMD GPUs (PCI vendor 0x1002) under a sysfs DRM root such
// as /sys/class/drm. Connector entries (card0-DP-1) and other vendors are
// skipped. The model name is not resolved; the PCI device id is reported
// instead. Indexes start at 0.
func amdSysfsGPUs(root string) []*mycelv1.Gpu {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var gpus []*mycelv1.Gpu
	for _, e := range entries { // ReadDir sorts by name
		if !drmCardRe.MatchString(e.Name()) {
			continue
		}
		dev := filepath.Join(root, e.Name(), "device")
		if readTrim(filepath.Join(dev, "vendor")) != "0x1002" {
			continue
		}
		g := &mycelv1.Gpu{Index: int32(len(gpus)), Vendor: "amd", Model: "AMD GPU"} //nolint:gosec // GPU count is tiny
		if id := readTrim(filepath.Join(dev, "device")); id != "" {
			g.Model = "AMD GPU (device " + id + ")"
		}
		if v, err := strconv.ParseUint(readTrim(filepath.Join(dev, "mem_info_vram_total")), 10, 64); err == nil {
			g.VramBytes = v
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
