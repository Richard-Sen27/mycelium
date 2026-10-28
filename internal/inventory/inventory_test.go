package inventory

import (
	"context"
	"encoding/binary"
	"runtime"
	"slices"
	"testing"
	"time"
	"unicode/utf16"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/version"
)

func TestProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	inv, err := Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("Probe took %v: %v", time.Since(start), inv)
	if inv.GetOs() != runtime.GOOS || inv.GetArch() != runtime.GOARCH {
		t.Errorf("os/arch = %q/%q", inv.GetOs(), inv.GetArch())
	}
	if inv.GetHostname() == "" {
		t.Error("empty hostname")
	}
	if inv.GetCpuCount() <= 0 {
		t.Errorf("cpu_count = %d", inv.GetCpuCount())
	}
	if inv.GetMemBytes() == 0 {
		t.Error("mem_bytes = 0")
	}
	if inv.GetMycelVersion() != version.Version {
		t.Errorf("mycel_version = %q", inv.GetMycelVersion())
	}
	if inv.GetDocker() == nil || inv.GetOllama() == nil {
		t.Error("docker/ollama presence must be non-nil")
	}
	ts, err := time.Parse(time.RFC3339, inv.GetProbedAt())
	if err != nil {
		t.Fatalf("probed_at %q: %v", inv.GetProbedAt(), err)
	}
	if _, off := ts.Zone(); off != 0 {
		t.Errorf("probed_at not UTC: %q", inv.GetProbedAt())
	}
	if d := time.Since(ts); d < -time.Minute || d > time.Minute {
		t.Errorf("probed_at %v is not now", ts)
	}
	if got := slices.Contains(inv.GetSandboxBackends(), BackendFake); got != fakeSandbox {
		t.Errorf("fake backend reported = %v, built with fakesandbox = %v", got, fakeSandbox)
	}
}

func TestProbeShortDeadline(t *testing.T) {
	for _, d := range []time.Duration{0, time.Millisecond, 50 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		start := time.Now()
		inv, _ := Probe(ctx) // a partial result or ErrNothingProbed are both fine
		cancel()
		if inv == nil {
			t.Fatalf("deadline %v: nil inventory", d)
		}
		if inv.GetOs() != runtime.GOOS || inv.GetProbedAt() == "" {
			t.Errorf("deadline %v: missing basics: %v", d, inv)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("deadline %v: Probe took %v", d, el)
		}
	}
}

func TestHash(t *testing.T) {
	base := &mycelv1.Inventory{
		Os: "darwin", Arch: "arm64", Hostname: "h", CpuCount: 8, MemBytes: 16 << 30,
		Gpus:            []*mycelv1.Gpu{{Index: 0, Vendor: "apple", Model: "Apple M1"}},
		Docker:          &mycelv1.ToolPresence{Present: true, Version: "29.0.0"},
		SandboxBackends: []string{"applevf"},
		ProbedAt:        "2026-01-01T00:00:00Z",
	}
	h := Hash(base)
	if len(h) != 64 {
		t.Fatalf("hash %q is not sha256 hex", h)
	}

	later, _ := proto.Clone(base).(*mycelv1.Inventory)
	later.ProbedAt = "2026-06-01T12:00:00Z"
	if Hash(later) != h {
		t.Error("hash changed with probed_at only")
	}
	if base.GetProbedAt() != "2026-01-01T00:00:00Z" {
		t.Error("Hash mutated its input")
	}

	more, _ := proto.Clone(base).(*mycelv1.Inventory)
	more.CpuCount = 16
	if Hash(more) == h {
		t.Error("hash did not change with cpu_count")
	}

	if Hash(nil) != Hash(&mycelv1.Inventory{}) {
		t.Error("nil and empty inventory should hash equal")
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []*mycelv1.Gpu
	}{
		{name: "empty", in: "", want: nil},
		{
			name: "single",
			in:   "0, NVIDIA GeForce RTX 4090, 24564, 550.54.15\n",
			want: []*mycelv1.Gpu{{Index: 0, Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090", VramBytes: 24564 << 20, Driver: "550.54.15"}},
		},
		{
			name: "two with CRLF and blank line",
			in:   "0, NVIDIA A100-SXM4-80GB, 81920, 535.104.05\r\n\r\n1, NVIDIA A100-SXM4-80GB, 81920, 535.104.05\r\n",
			want: []*mycelv1.Gpu{
				{Index: 0, Vendor: "nvidia", Model: "NVIDIA A100-SXM4-80GB", VramBytes: 81920 << 20, Driver: "535.104.05"},
				{Index: 1, Vendor: "nvidia", Model: "NVIDIA A100-SXM4-80GB", VramBytes: 81920 << 20, Driver: "535.104.05"},
			},
		},
		{
			name: "memory N/A",
			in:   "0, NVIDIA Graphics Device, [N/A], [N/A]\n",
			want: []*mycelv1.Gpu{{Index: 0, Vendor: "nvidia", Model: "NVIDIA Graphics Device"}},
		},
		{name: "garbage", in: "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGPUs(t, parseNvidiaSMI(tt.in), tt.want)
		})
	}
}

const spAppleSilicon = `{
  "SPDisplaysDataType" : [
    {
      "_name" : "kHW_AppleM1Item",
      "spdisplays_mtlgpufamilysupport" : "spdisplays_metal4",
      "spdisplays_ndrvs" : [ { "_name" : "Color LCD" } ],
      "spdisplays_vendor" : "sppci_vendor_Apple",
      "sppci_bus" : "spdisplays_builtin",
      "sppci_cores" : "8",
      "sppci_device_type" : "spdisplays_gpu",
      "sppci_model" : "Apple M1"
    }
  ]
}`

const spIntelMacAMD = `{
  "SPDisplaysDataType" : [
    {
      "_name" : "Intel UHD Graphics 630",
      "spdisplays_vendor" : "Intel",
      "spdisplays_vram_shared" : "1536 MB",
      "sppci_bus" : "spdisplays_builtin",
      "sppci_device_type" : "spdisplays_gpu",
      "sppci_model" : "Intel UHD Graphics 630"
    },
    {
      "_name" : "AMD Radeon Pro 5500M",
      "spdisplays_vendor" : "sppci_vendor_amd",
      "spdisplays_vram" : "4 GB",
      "sppci_bus" : "spdisplays_pcie_device",
      "sppci_device_type" : "spdisplays_gpu",
      "sppci_model" : "AMD Radeon Pro 5500M"
    }
  ]
}`

func TestParseSystemProfiler(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		start   int32
		want    []*mycelv1.Gpu
		wantErr bool
	}{
		{
			name: "apple silicon",
			in:   spAppleSilicon,
			want: []*mycelv1.Gpu{{Index: 0, Vendor: "apple", Model: "Apple M1"}},
		},
		{
			name: "intel mac with discrete amd",
			in:   spIntelMacAMD,
			want: []*mycelv1.Gpu{
				{Index: 0, Vendor: "intel", Model: "Intel UHD Graphics 630"},
				{Index: 1, Vendor: "amd", Model: "AMD Radeon Pro 5500M", VramBytes: 4 << 30},
			},
		},
		{
			name:  "index offset after nvidia",
			in:    spAppleSilicon,
			start: 2,
			want:  []*mycelv1.Gpu{{Index: 2, Vendor: "apple", Model: "Apple M1"}},
		},
		{
			name: "name fallback and MB vram",
			in:   `{"SPDisplaysDataType":[{"_name":"AMD Radeon RX 580","spdisplays_vendor":"AMD (0x1002)","spdisplays_vram":"8192 MB"}]}`,
			want: []*mycelv1.Gpu{{Index: 0, Vendor: "amd", Model: "AMD Radeon RX 580", VramBytes: 8 << 30}},
		},
		{name: "empty list", in: `{"SPDisplaysDataType":[]}`, want: nil},
		{name: "invalid json", in: `not json`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSystemProfiler([]byte(tt.in), tt.start)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			assertGPUs(t, got, tt.want)
		})
	}
}

func utf16LE(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func TestParseWSLList(t *testing.T) {
	const list = "Ubuntu-24.04\r\nmycel-sbx\r\ndocker-desktop\r\n"
	want := []string{"Ubuntu-24.04", "mycel-sbx", "docker-desktop"}
	tests := []struct {
		name string
		in   []byte
		want []string
	}{
		{name: "utf16le with bom", in: utf16LE(list, true), want: want},
		{name: "utf16le without bom", in: utf16LE(list, false), want: want},
		{name: "utf8", in: []byte(list), want: want},
		{name: "utf8 with bom, LF", in: []byte("\xef\xbb\xbfUbuntu-24.04\nmycel-sbx\n"), want: want[:2]},
		{name: "utf16 trailing nul", in: append(utf16LE("mycel-sbx\r\n", true), 0, 0), want: []string{"mycel-sbx"}},
		{name: "empty", in: nil, want: nil},
		{name: "no distros utf16", in: utf16LE("\r\n", true), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseWSLList(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseOllamaVersion(t *testing.T) {
	tests := map[string]string{
		"ollama version is 0.5.7\n": "0.5.7",
		"Warning: could not connect to a running Ollama instance\nWarning: client version is 0.40.1\n": "0.40.1",
		"": "",
	}
	for in, want := range tests {
		if got := parseOllamaVersion(in); got != want {
			t.Errorf("parseOllamaVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := map[string]uint64{"4 GB": 4 << 30, "1536 MB": 1536 << 20, "8GB": 8 << 30, "": 0, "lots": 0, "512": 512}
	for in, want := range tests {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func assertGPUs(t *testing.T, got, want []*mycelv1.Gpu) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d GPUs %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("gpu %d = %v, want %v", i, got[i], want[i])
		}
	}
}
