package sandbox

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

type stubBackend struct {
	name string
	ok   bool
	why  string
}

func (b stubBackend) Name() string                                        { return b.name }
func (b stubBackend) Available(context.Context) (bool, string)            { return b.ok, b.why }
func (b stubBackend) Capabilities() Capabilities                          { return Capabilities{} }
func (b stubBackend) Prepare(context.Context, string, func(string)) error { return nil }
func (b stubBackend) Create(context.Context, Spec) (Sandbox, error) {
	return nil, errors.New("stub")
}

func withRegistry(t *testing.T, bs ...Backend) {
	t.Helper()
	saved := registry
	registry = bs
	t.Cleanup(func() { registry = saved })
}

func TestSelect(t *testing.T) {
	withRegistry(t,
		stubBackend{name: BackendGVisor, ok: false, why: "runsc not found"},
		stubBackend{name: BackendFirecracker, ok: true},
		stubBackend{name: BackendFake, ok: true},
	)
	ctx := context.Background()
	cases := []struct {
		name     string
		backends []string
		want     string
		reasons  []string
	}{
		{"first available", []string{BackendGVisor, BackendFirecracker}, BackendFirecracker, nil},
		{"preference order", []string{BackendFirecracker, BackendFake}, BackendFirecracker, nil},
		{"fake only when listed", []string{BackendAppleVF, BackendGVisor}, "", []string{"applevf: not compiled", "gvisor: runsc not found"}},
		{"fake listed", []string{BackendFake}, BackendFake, nil},
		{"empty list", nil, "", []string{"no backends"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := Select(ctx, &mycelv1.Profile{Name: "p", Backends: c.backends})
			if c.want != "" {
				if err != nil || b.Name() != c.want {
					t.Fatalf("Select = %v, %v; want %s", b, err, c.want)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			for _, r := range c.reasons {
				if !strings.Contains(err.Error(), r) {
					t.Errorf("err %q lacks %q", err, r)
				}
			}
		})
	}
	if _, err := Select(ctx, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil profile: %v", err)
	}
}

func TestResolveLimits(t *testing.T) {
	defaults := Limits{CPUs: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30, MaxProcesses: 512, WallClock: time.Hour}
	prof := &mycelv1.Limits{Cpus: 4, MemoryBytes: 8 << 30, WallClock: "2h"}
	cases := []struct {
		name          string
		profile, over *mycelv1.Limits
		want          Limits
		err           string
	}{
		{"defaults", nil, nil, defaults, ""},
		{"zero profile", &mycelv1.Limits{}, nil, defaults, ""},
		{"profile values", prof, nil, Limits{CPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 20 << 30, MaxProcesses: 512, WallClock: 2 * time.Hour}, ""},
		{"lower", prof, &mycelv1.Limits{Cpus: 1, MaxProcesses: 10, WallClock: "30m"}, Limits{CPUs: 1, MemoryBytes: 8 << 30, DiskBytes: 20 << 30, MaxProcesses: 10, WallClock: 30 * time.Minute}, ""},
		{"equal is fine", prof, &mycelv1.Limits{Cpus: 4}, Limits{CPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 20 << 30, MaxProcesses: 512, WallClock: 2 * time.Hour}, ""},
		{"raise cpus", prof, &mycelv1.Limits{Cpus: 8}, Limits{}, "cpus"},
		{"raise memory", prof, &mycelv1.Limits{MemoryBytes: 16 << 30}, Limits{}, "memory_bytes"},
		{"raise default disk", nil, &mycelv1.Limits{DiskBytes: 21 << 30}, Limits{}, "disk_bytes"},
		{"raise wall clock", prof, &mycelv1.Limits{WallClock: "3h"}, Limits{}, "wall_clock"},
		{"bad override duration", prof, &mycelv1.Limits{WallClock: "soon"}, Limits{}, "wall_clock"},
		{"zero duration", prof, &mycelv1.Limits{WallClock: "0s"}, Limits{}, "positive"},
		{"bad profile duration", &mycelv1.Limits{WallClock: "x"}, nil, Limits{}, "wall_clock"},
		{"negative", nil, &mycelv1.Limits{Cpus: -1}, Limits{}, "negative"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveLimits(c.profile, c.over)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func TestCapWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &capWriter{w: &buf, max: 10}
	for _, s := range []string{"abcd", "efgh", "ijkl", "mnop"} {
		if n, err := w.Write([]byte(s)); n != 4 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got, want := buf.String(), "abcdefghij"+LogTruncatedMarker; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCheckRelPattern(t *testing.T) {
	for p, ok := range map[string]bool{
		"out/*.txt": true, "**/*.log": true, "a": true,
		"": false, "/etc/passwd": false, "../x": false, "a/../../b": false, `a\b`: false,
	} {
		if err := checkRelPattern(p); (err == nil) != ok {
			t.Errorf("checkRelPattern(%q) = %v", p, err)
		}
	}
}
