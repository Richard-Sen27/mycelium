// Package sharing implements the node's sharing settings (spec: "Sharing
// settings per node", "Node owner has the final say"): what the node offers,
// when (always, idle, schedule), local-first priority and the job cap. The
// hub holds the owner's settings; the node keeps a local ceiling in
// <NodeDir>/sharing.yaml that may only tighten them and that it enforces even
// while the hub is unreachable.
//
// Contract (owned by the orchestrator).
package sharing

import (
	"errors"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("sharing: not implemented")

// ErrWider is returned when a local change would widen the ceiling.
var ErrWider = errors.New("sharing: local settings may only tighten")

// Known offer names.
var Offers = []string{"docker", "ollama", "gpu", "sandbox"}

// Default returns the settings used when neither hub nor node set any:
// offer everything in the capability ceiling, always, local_first true,
// max_jobs 2, idle_after 10.
func Default(ceiling []string) *mycelv1.SharingSettings { panic(ErrNotImplemented) }

// Validate checks names, windows ("HH:MM" local, days mon..sun) and ranges.
func Validate(s *mycelv1.SharingSettings) error { panic(ErrNotImplemented) }

// Effective returns the intersection of the hub-side settings and the local
// ceiling: offers = intersection; when = the stricter of always < schedule
// < idle (a schedule intersects windows; idle wins over schedule); local_first
// = either; max_jobs = min of non-zero; idle_after = max.
func Effective(hub, local *mycelv1.SharingSettings) *mycelv1.SharingSettings {
	panic(ErrNotImplemented)
}

// Tightens reports whether candidate is equal to or stricter than base in
// every field, i.e. a valid local change.
func Tightens(base, candidate *mycelv1.SharingSettings) bool { panic(ErrNotImplemented) }

// ActiveAt reports whether sharing is active at t for settings s given the
// owner's idle state (seconds since last input, from the idle detector).
func ActiveAt(s *mycelv1.SharingSettings, t time.Time, idleFor time.Duration) bool {
	panic(ErrNotImplemented)
}

// IdleDetector reports how long the machine's owner has been idle (no
// keyboard or mouse input). Platform implementations: macOS via
// CGEventSourceSecondsSinceLastEventType through IOKit/HID idle time
// (ioreg HIDIdleTime), Windows via GetLastInputInfo, Linux via
// /dev/input event timestamps or X11 screensaver extension; a backend that
// cannot tell reports 0 (never idle).
type IdleDetector interface {
	IdleFor() (time.Duration, error)
}

// NewIdleDetector returns the platform detector.
func NewIdleDetector() IdleDetector { panic(ErrNotImplemented) }

// Load reads <NodeDir>/sharing.yaml; a missing file yields nil (no local ceiling).
func Load(nodeDir string) (*mycelv1.SharingSettings, error) { panic(ErrNotImplemented) }

// Save writes <NodeDir>/sharing.yaml atomically (0600).
func Save(nodeDir string, s *mycelv1.SharingSettings) error { panic(ErrNotImplemented) }
