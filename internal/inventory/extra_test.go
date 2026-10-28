package inventory

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func TestParseWSLVerbose(t *testing.T) {
	const listV = "  NAME            STATE           VERSION\r\n" +
		"* Ubuntu-24.04    Running         2\r\n" +
		"  MYCEL-SBX       Stopped         2\r\n" +
		"  docker-desktop  Stopped         2\r\n"
	const wsl1 = "  NAME            STATE           VERSION\r\n" +
		"* Ubuntu          Running         2\r\n" +
		"  mycel-sbx       Stopped         1\r\n"
	const defaultSbx = "  NAME      STATE     VERSION\n* mycel-sbx Running   2\n"
	all := []wslDistro{{"Ubuntu-24.04", "2"}, {"MYCEL-SBX", "2"}, {"docker-desktop", "2"}}
	tests := []struct {
		name     string
		in       []byte
		want     []wslDistro
		wantSbx2 bool
	}{
		{name: "utf16le with bom", in: utf16LE(listV, true), want: all, wantSbx2: true},
		{name: "utf8", in: []byte(listV), want: all, wantSbx2: true},
		{
			name: "mycel-sbx on WSL1 does not count (utf16)",
			in:   utf16LE(wsl1, true),
			want: []wslDistro{{"Ubuntu", "2"}, {"mycel-sbx", "1"}},
		},
		{
			name: "mycel-sbx on WSL1 does not count (utf8)",
			in:   []byte(wsl1),
			want: []wslDistro{{"Ubuntu", "2"}, {"mycel-sbx", "1"}},
		},
		{
			name:     "mycel-sbx is the default distro",
			in:       utf16LE(defaultSbx, true),
			want:     []wslDistro{{"mycel-sbx", "2"}},
			wantSbx2: true,
		},
		{name: "header only", in: utf16LE("  NAME  STATE  VERSION\r\n", true), want: nil},
		{name: "empty", in: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWSLVerbose(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if has := hasWSL2Distro(got, "mycel-sbx"); has != tt.wantSbx2 {
				t.Errorf("hasWSL2Distro = %v, want %v", has, tt.wantSbx2)
			}
		})
	}
}

func TestAMDSysfsGPUs(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("card0/device/vendor", "0x1002")
	write("card0/device/device", "0x744c")
	write("card0/device/mem_info_vram_total", "25753026560")
	write("card1/device/vendor", "0x10de") // NVIDIA: skipped
	write("card0-DP-1/device/vendor", "0x1002")
	write("card0-DP-1/status", "connected") // connector: skipped
	write("card2/device/vendor", "0x1002")  // AMD without device id or VRAM

	want := []*mycelv1.Gpu{
		{Index: 0, Vendor: "amd", Model: "AMD GPU (device 0x744c)", VramBytes: 25753026560},
		{Index: 1, Vendor: "amd", Model: "AMD GPU"},
	}
	assertGPUs(t, amdSysfsGPUs(root), want)

	if g := amdSysfsGPUs(filepath.Join(root, "missing")); g != nil {
		t.Errorf("missing root: got %v", g)
	}
}

func blockingCmd() (string, []string) {
	if runtime.GOOS == "windows" {
		return "ping", []string{"-n", "30", "127.0.0.1"}
	}
	return "sleep", []string{"30"}
}

func TestRunCmdTimeout(t *testing.T) {
	name, args := blockingCmd()
	check := func(t *testing.T, ctx context.Context) {
		t.Helper()
		start := time.Now()
		if _, err := runCmd(ctx, name, args...); err == nil {
			t.Error("expected an error from a killed command")
		}
		if el := time.Since(start); el > time.Second {
			t.Errorf("runCmd took %v", el)
		}
	}

	t.Run("ctx deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		check(t, ctx)
	})

	t.Run("package timeout", func(t *testing.T) {
		old := cmdTimeout
		cmdTimeout = 100 * time.Millisecond
		t.Cleanup(func() { cmdTimeout = old })
		check(t, context.Background())
	})
}
