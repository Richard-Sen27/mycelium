package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Default limits applied when a profile leaves a value at zero.
const (
	DefaultCPUs         = 2
	DefaultMemoryBytes  = 4 << 30
	DefaultDiskBytes    = 20 << 30
	DefaultMaxProcesses = 512
	DefaultWallClock    = time.Hour
)

// WorkspaceGuestPath is where the workspace lives inside every sandbox.
const WorkspaceGuestPath = "/workspace"

func selectBackend(ctx context.Context, profile *mycelv1.Profile) (Backend, error) {
	if profile == nil {
		return nil, fmt.Errorf("%w: no profile", ErrUnavailable)
	}
	if len(profile.GetBackends()) == 0 {
		return nil, fmt.Errorf("%w: profile %q lists no backends", ErrUnavailable, profile.GetName())
	}
	var reasons []string
	for _, name := range profile.GetBackends() {
		var b Backend
		for _, r := range registry {
			if r.Name() == name {
				b = r
				break
			}
		}
		if b == nil {
			reasons = append(reasons, name+": not compiled into this binary")
			continue
		}
		ok, why := b.Available(ctx)
		if ok {
			return b, nil
		}
		if why == "" {
			why = "not available"
		}
		reasons = append(reasons, name+": "+why)
	}
	return nil, fmt.Errorf("%w for profile %q: %s", ErrUnavailable, profile.GetName(), strings.Join(reasons, "; "))
}

func resolveLimits(profile, override *mycelv1.Limits) (Limits, error) {
	l := Limits{
		CPUs:         DefaultCPUs,
		MemoryBytes:  DefaultMemoryBytes,
		DiskBytes:    DefaultDiskBytes,
		MaxProcesses: DefaultMaxProcesses,
		WallClock:    DefaultWallClock,
	}
	if err := checkLimits("profile", profile); err != nil {
		return Limits{}, err
	}
	if err := checkLimits("override", override); err != nil {
		return Limits{}, err
	}
	if profile != nil {
		if v := profile.GetCpus(); v > 0 {
			l.CPUs = int(v)
		}
		if v := profile.GetMemoryBytes(); v > 0 {
			l.MemoryBytes = v
		}
		if v := profile.GetDiskBytes(); v > 0 {
			l.DiskBytes = v
		}
		if v := profile.GetMaxProcesses(); v > 0 {
			l.MaxProcesses = int(v)
		}
		if w := profile.GetWallClock(); w != "" {
			l.WallClock, _ = time.ParseDuration(w) // checked above
		}
	}
	if override == nil {
		return l, nil
	}
	lower := func(field string, have, want int64) (int64, error) {
		if want == 0 {
			return have, nil
		}
		if want > have {
			return 0, fmt.Errorf("sandbox: limit %s %d exceeds the profile's %d", field, want, have)
		}
		return want, nil
	}
	var err error
	var v int64
	if v, err = lower("cpus", int64(l.CPUs), int64(override.GetCpus())); err != nil {
		return Limits{}, err
	}
	l.CPUs = int(v)
	if l.MemoryBytes, err = lower("memory_bytes", l.MemoryBytes, override.GetMemoryBytes()); err != nil {
		return Limits{}, err
	}
	if l.DiskBytes, err = lower("disk_bytes", l.DiskBytes, override.GetDiskBytes()); err != nil {
		return Limits{}, err
	}
	if v, err = lower("max_processes", int64(l.MaxProcesses), int64(override.GetMaxProcesses())); err != nil {
		return Limits{}, err
	}
	l.MaxProcesses = int(v)
	if w := override.GetWallClock(); w != "" {
		d, _ := time.ParseDuration(w) // checked above
		if d > l.WallClock {
			return Limits{}, fmt.Errorf("sandbox: limit wall_clock %s exceeds the profile's %s", d, l.WallClock)
		}
		l.WallClock = d
	}
	return l, nil
}

func checkLimits(which string, l *mycelv1.Limits) error {
	if l == nil {
		return nil
	}
	if l.GetCpus() < 0 || l.GetMemoryBytes() < 0 || l.GetDiskBytes() < 0 || l.GetMaxProcesses() < 0 || l.GetMaxTokens() < 0 {
		return fmt.Errorf("sandbox: %s limits must not be negative", which)
	}
	if w := l.GetWallClock(); w != "" {
		d, err := time.ParseDuration(w)
		if err != nil {
			return fmt.Errorf("sandbox: %s wall_clock: %w", which, err)
		}
		if d <= 0 {
			return fmt.Errorf("sandbox: %s wall_clock %q must be positive", which, w)
		}
	}
	return nil
}
