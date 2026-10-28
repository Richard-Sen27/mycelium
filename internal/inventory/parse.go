package inventory

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf16"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Parsers live in an untagged file so they are tested on every OS.

// nvidiaGPUs queries nvidia-smi; it returns nil when the tool is absent or fails.
func nvidiaGPUs(ctx context.Context) []*mycelv1.Gpu {
	p, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	out, err := runCmd(ctx, p, "--query-gpu=index,name,memory.total,driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}

// parseNvidiaSMI parses
// `nvidia-smi --query-gpu=index,name,memory.total,driver_version --format=csv,noheader,nounits`
// output; memory.total is in MiB. Malformed lines are skipped.
func parseNvidiaSMI(out string) []*mycelv1.Gpu {
	var gpus []*mycelv1.Gpu
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		// Names never contain commas in practice, but be tolerant: the last
		// two fields are memory and driver, the first is the index.
		idx, err := strconv.ParseInt(f[0], 10, 32)
		if err != nil {
			continue
		}
		name := strings.Join(f[1:len(f)-2], ",")
		g := &mycelv1.Gpu{
			Index:  int32(idx),
			Vendor: "nvidia",
			Model:  name,
			Driver: naToEmpty(f[len(f)-1]),
		}
		if mib, err := strconv.ParseUint(f[len(f)-2], 10, 64); err == nil {
			g.VramBytes = mib << 20
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func naToEmpty(s string) string {
	if strings.Contains(s, "N/A") {
		return ""
	}
	return s
}

// systemProfilerDisplays mirrors the parts of
// `system_profiler SPDisplaysDataType -json` we use.
type systemProfilerDisplays struct {
	Items []struct {
		Name       string `json:"_name"`
		Model      string `json:"sppci_model"`
		Vendor     string `json:"spdisplays_vendor"`
		PCIVendor  string `json:"sppci_vendor"`
		VRAM       string `json:"spdisplays_vram"`
		DeviceType string `json:"sppci_device_type"`
	} `json:"SPDisplaysDataType"`
}

// parseSystemProfiler parses `system_profiler SPDisplaysDataType -json`.
// Apple Silicon GPUs use unified memory, so their VRAM is reported as 0 and
// left to mem_bytes; discrete GPUs report spdisplays_vram. Indexes start at
// startIndex.
func parseSystemProfiler(data []byte, startIndex int32) ([]*mycelv1.Gpu, error) {
	var sp systemProfilerDisplays
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, err
	}
	var gpus []*mycelv1.Gpu
	idx := startIndex
	for _, it := range sp.Items {
		if it.DeviceType != "" && it.DeviceType != "spdisplays_gpu" {
			continue
		}
		model := it.Model
		if model == "" {
			model = it.Name
		}
		vendor := gpuVendor(it.Vendor + " " + it.PCIVendor + " " + model)
		g := &mycelv1.Gpu{Index: idx, Vendor: vendor, Model: model}
		if vendor != "apple" {
			g.VramBytes = parseSize(it.VRAM)
		}
		gpus = append(gpus, g)
		idx++
	}
	return gpus, nil
}

// gpuVendor maps free-form vendor text to nvidia, amd, apple or intel.
func gpuVendor(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "apple"):
		return "apple"
	case strings.Contains(s, "nvidia"), strings.Contains(s, "0x10de"):
		return "nvidia"
	case strings.Contains(s, "amd"), strings.Contains(s, "ati technologies"), strings.Contains(s, "radeon"), strings.Contains(s, "0x1002"):
		return "amd"
	case strings.Contains(s, "intel"), strings.Contains(s, "0x8086"):
		return "intel"
	}
	return ""
}

// parseSize parses sizes such as "4 GB", "1536 MB" or "8GB" into bytes.
func parseSize(s string) uint64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := uint64(1)
	for _, u := range []struct {
		suffix string
		mult   uint64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0
	}
	return uint64(n * float64(mult))
}

// parseWSLList parses `wsl.exe -l -q` output into distro names. Real
// wsl.exe writes UTF-16LE (with or without a BOM); plain UTF-8 is accepted
// too (WSL_UTF8=1).
func parseWSLList(b []byte) []string {
	text := decodeMaybeUTF16(b)
	var names []string
	for line := range strings.Lines(text) {
		line = strings.TrimPrefix(strings.Trim(line, " \t\r\n\x00"), utf8BOM)
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

func decodeMaybeUTF16(b []byte) string {
	hasBOM := len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE
	if !hasBOM && !looksUTF16LE(b) {
		return strings.TrimPrefix(string(b), utf8BOM)
	}
	if hasBOM {
		b = b[2:]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:]))
	}
	return string(utf16.Decode(u))
}

// looksUTF16LE guesses BOM-less UTF-16LE: mostly-ASCII text has a zero in
// almost every odd byte.
func looksUTF16LE(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	zeros, pairs := 0, 0
	for i := 1; i < len(b); i += 2 {
		pairs++
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros*2 > pairs
}

// utf8BOM is the UTF-8 encoding of U+FEFF.
const utf8BOM = "\xef\xbb\xbf"

// wslDistro is one row of `wsl.exe -l -v`.
type wslDistro struct {
	Name    string
	Version string // WSL version: "1" or "2"
}

// parseWSLVerbose parses `wsl.exe -l -v` output (UTF-16LE or UTF-8):
//
//	  NAME            STATE           VERSION
//	* Ubuntu          Running         2
//	  mycel-sbx       Stopped         2
//
// The header is skipped and the "*" default marker is dropped. Distro names
// cannot contain spaces, so name and version are the first and last fields.
func parseWSLVerbose(b []byte) []wslDistro {
	var out []wslDistro
	for _, line := range parseWSLList(b) {
		f := strings.Fields(strings.TrimPrefix(line, "*"))
		if len(f) < 3 || (f[0] == "NAME" && f[len(f)-1] == "VERSION") {
			continue
		}
		out = append(out, wslDistro{Name: f[0], Version: f[len(f)-1]})
	}
	return out
}

// hasWSL2Distro reports whether name (case-insensitive) is a WSL 2 distro.
func hasWSL2Distro(distros []wslDistro, name string) bool {
	for _, d := range distros {
		if strings.EqualFold(d.Name, name) && d.Version == "2" {
			return true
		}
	}
	return false
}
