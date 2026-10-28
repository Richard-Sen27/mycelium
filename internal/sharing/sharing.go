// Package sharing implements the node's sharing settings (spec: "Sharing
// settings per node", "Node owner has the final say"): what the node offers,
// when (always, idle, schedule), local-first priority and the job cap. The
// hub holds the owner's settings; the node keeps a local ceiling in
// <NodeDir>/sharing.yaml that may only tighten them and that it enforces even
// while the hub is unreachable.
//
// Conventions shared by every function here:
//   - a nil Offer means "no restriction" (every known offer); a non-nil Offer
//     with no names offers nothing;
//   - When "" means "always"; an unknown When is never active;
//   - When "schedule" is active inside its windows; When "idle" is active
//     once the owner has been idle for IdleAfterMinutes and, when it carries
//     windows, only inside them ("idle AND inside the windows");
//   - IdleAfterMinutes 0 means the default (10) on hub-side settings and "no
//     extra limit" on a local ceiling; MaxJobs 0 likewise means the default
//     (2) on hub-side settings and "no extra limit" on a local ceiling;
//   - schedule windows are local wall-clock time, end-exclusive; "to" may be
//     "24:00" (end of day) and a window with to < from runs overnight into
//     the next day (sun wraps to mon).
//
// Contract (owned by the orchestrator).
package sharing

import (
	"errors"
	"fmt"
	"slices"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("sharing: not implemented")

// ErrWider is returned when a local change would widen the ceiling.
var ErrWider = errors.New("sharing: local settings may only tighten")

// Known offer names.
var Offers = []string{"docker", "ollama", "gpu", "sandbox"}

// Values of SharingSettings.When.
const (
	WhenAlways   = "always"
	WhenIdle     = "idle"
	WhenSchedule = "schedule"
)

// Defaults and limits.
const (
	DefaultMaxJobs   = 2
	DefaultIdleAfter = 10 // minutes
	MaxWindows       = 32
	MaxJobsLimit     = 64
	MaxIdleAfter     = 1440 // minutes
)

// Default returns the settings used when neither hub nor node set any:
// offer everything in the capability ceiling, always, local_first true,
// max_jobs 2, idle_after 10.
func Default(ceiling []string) *mycelv1.SharingSettings {
	return &mycelv1.SharingSettings{
		Offer:            &mycelv1.Capabilities{Names: intersectOffers(Offers, ceiling)},
		When:             WhenAlways,
		LocalFirst:       true,
		MaxJobs:          DefaultMaxJobs,
		IdleAfterMinutes: DefaultIdleAfter,
	}
}

// Validate checks names, windows ("HH:MM" local, days mon..sun) and ranges.
// "schedule" needs at least one window; "idle" may carry windows (idle AND
// inside them); "always" carries none. A nil settings value is valid (no
// settings).
func Validate(s *mycelv1.SharingSettings) error {
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, n := range s.GetOffer().GetNames() {
		if !slices.Contains(Offers, n) {
			return fmt.Errorf("sharing: unknown offer %q (want one of %v)", n, Offers)
		}
		if seen[n] {
			return fmt.Errorf("sharing: duplicate offer %q", n)
		}
		seen[n] = true
	}
	switch s.GetWhen() {
	case "", WhenAlways:
		if len(s.GetSchedule()) > 0 {
			return errors.New(`sharing: schedule windows need when "schedule" or "idle"`)
		}
	case WhenSchedule:
		if len(s.GetSchedule()) == 0 {
			return errors.New(`sharing: when "schedule" needs at least one window`)
		}
	case WhenIdle:
	default:
		return fmt.Errorf("sharing: when %q must be always, idle or schedule", s.GetWhen())
	}
	if len(s.GetSchedule()) > MaxWindows {
		return fmt.Errorf("sharing: at most %d schedule windows, got %d", MaxWindows, len(s.GetSchedule()))
	}
	for i, w := range s.GetSchedule() {
		if err := validateWindow(w); err != nil {
			return fmt.Errorf("sharing: schedule window %d: %w", i, err)
		}
	}
	if s.GetMaxJobs() < 0 || s.GetMaxJobs() > MaxJobsLimit {
		return fmt.Errorf("sharing: max_jobs %d out of range 0..%d", s.GetMaxJobs(), MaxJobsLimit)
	}
	if s.GetIdleAfterMinutes() < 0 || s.GetIdleAfterMinutes() > MaxIdleAfter {
		return fmt.Errorf("sharing: idle_after_minutes %d out of range 0..%d", s.GetIdleAfterMinutes(), MaxIdleAfter)
	}
	return nil
}

// Effective returns the conjunction of the hub-side settings and the local
// ceiling: sharing is active only when both sides allow it. Offers are the
// intersection; a schedule on either side keeps the intersected windows; an
// idle requirement on either side makes the result require idle (with the
// larger idle_after); local_first = either; max_jobs = min of non-zero.
// "idle AND inside the windows" is When "idle" with a non-empty Schedule.
//
// A nil hub stands for Default(Offers); a nil local is no ceiling, so the
// result is the defaulted hub settings. The result always has Offer, When,
// MaxJobs and IdleAfterMinutes set. When nothing can be active (disjoint
// schedules) the result is When "schedule" with no windows, which ActiveAt
// treats as never active (and Validate rejects as input). If either input
// fails Validate the result offers nothing and is never active.
func Effective(hub, local *mycelv1.SharingSettings) *mycelv1.SharingSettings {
	if Validate(hub) != nil || Validate(local) != nil {
		return &mycelv1.SharingSettings{
			Offer:            &mycelv1.Capabilities{Names: []string{}},
			When:             WhenSchedule,
			LocalFirst:       true,
			MaxJobs:          DefaultMaxJobs,
			IdleAfterMinutes: DefaultIdleAfter,
		}
	}
	h := withDefaults(hub)
	out := &mycelv1.SharingSettings{
		Offer:            &mycelv1.Capabilities{Names: intersectOffers(offerSet(h), offerSet(local))},
		LocalFirst:       h.GetLocalFirst() || local.GetLocalFirst(),
		MaxJobs:          h.GetMaxJobs(),
		IdleAfterMinutes: max(h.GetIdleAfterMinutes(), local.GetIdleAfterMinutes()),
	}
	if m := local.GetMaxJobs(); m > 0 && m < out.MaxJobs {
		out.MaxJobs = m
	}
	a := activityOf(h).and(activityOf(local))
	switch {
	case a.empty():
		out.When = WhenSchedule
	case a.idle:
		out.When = WhenIdle
		if a.w != nil {
			out.Schedule = a.w.windows()
		}
	case a.w != nil:
		out.When = WhenSchedule
		out.Schedule = a.w.windows()
	default:
		out.When = WhenAlways
	}
	return out
}

// Tightens reports whether candidate is equal to or stricter than base in
// every field, i.e. a valid local change: for every hub setting H,
// Effective(H, candidate) is within Effective(H, base). Both are read as
// local ceilings, so a zero field (nil Offer, When "", no windows on "idle",
// local_first false, max_jobs 0, idle_after 0) means "no extra limit" on
// either side and nil means no ceiling at all. Field by field: the
// candidate's offers ⊆ base's; its active set (every minute of the week,
// idle or not) ⊆ base's, so a base schedule needs the candidate's windows ⊆
// base's and a base idle requirement needs the candidate to require idle,
// while an unknown When (never active) is the strictest; local_first stays
// on if base has it; max_jobs ≤ base's when base sets one; idle_after ≥
// base's. The relation is reflexive and transitive. To check against
// hub-side settings, whose zeros mean defaults, pass Effective(hub, nil).
func Tightens(base, candidate *mycelv1.SharingSettings) bool {
	bo := offerSet(base)
	for _, o := range offerSet(candidate) {
		if !slices.Contains(bo, o) {
			return false
		}
	}
	if !activityOf(candidate).subsetOf(activityOf(base)) {
		return false
	}
	if base.GetLocalFirst() && !candidate.GetLocalFirst() {
		return false
	}
	if b := base.GetMaxJobs(); b > 0 && (candidate.GetMaxJobs() <= 0 || candidate.GetMaxJobs() > b) {
		return false
	}
	return candidate.GetIdleAfterMinutes() >= base.GetIdleAfterMinutes()
}

// ActiveAt reports whether sharing is active at t for settings s given the
// owner's idle state (time since last input, from the idle detector).
//
// Windows are evaluated on the wall clock of t.In(time.Local), the node's
// local time. "idle" needs idleFor >= idle_after (0 = 10 minutes) and, when
// it carries windows, t inside them; "schedule" needs t inside a window (no
// windows = never); "always" and nil settings are always active; an unknown
// When is never active.
func ActiveAt(s *mycelv1.SharingSettings, t time.Time, idleFor time.Duration) bool {
	m := minuteOfWeek(t.In(time.Local))
	switch whenOf(s) {
	case WhenAlways:
		return true
	case WhenSchedule:
		return inWindows(s.GetSchedule(), m)
	case WhenIdle:
		after := s.GetIdleAfterMinutes()
		if after <= 0 {
			after = DefaultIdleAfter
		}
		if idleFor < time.Duration(after)*time.Minute {
			return false
		}
		return len(s.GetSchedule()) == 0 || inWindows(s.GetSchedule(), m)
	default:
		return false
	}
}

// IdleDetector reports how long the machine's owner has been idle (no
// keyboard or mouse input). Platform implementations: macOS via the IOKit
// HID idle time (ioreg HIDIdleTime), Windows via GetLastInputInfo (the
// daemon must run in the owner's interactive session), Linux via logind's
// IdleSinceHint for the active session or xprintidle. A single call takes
// at most IdleTimeout. Every implementation here fails safe: when it cannot
// tell it logs a warning and reports 0 (never idle) with a nil error.
type IdleDetector interface {
	IdleFor() (time.Duration, error)
}

// activity is the set of (minute of week, owner idle) pairs a setting
// allows: never, or idle-required and/or restricted to the minutes in w
// (nil = every minute).
type activity struct {
	never bool
	idle  bool
	w     *week
}

func activityOf(s *mycelv1.SharingSettings) activity {
	switch whenOf(s) {
	case WhenAlways:
		return activity{}
	case WhenSchedule:
		return activity{w: weekOf(s.GetSchedule())}
	case WhenIdle:
		a := activity{idle: true}
		if len(s.GetSchedule()) > 0 {
			a.w = weekOf(s.GetSchedule())
		}
		return a
	default:
		return activity{never: true}
	}
}

func (a activity) empty() bool { return a.never || (a.w != nil && a.w.empty()) }

func (a activity) and(b activity) activity {
	out := activity{never: a.never || b.never, idle: a.idle || b.idle}
	switch {
	case a.w == nil:
		out.w = b.w
	case b.w == nil:
		out.w = a.w
	default:
		out.w = a.w.and(b.w)
	}
	return out
}

func (a activity) subsetOf(b activity) bool {
	if a.empty() {
		return true
	}
	if b.empty() || (b.idle && !a.idle) {
		return false
	}
	return b.w == nil || (a.w != nil && a.w.subsetOf(b.w))
}

// withDefaults returns hub-side settings with zero values defaulted: nil →
// Default(Offers); nil Offer → every known offer; "" when → always; 0
// max_jobs → 2; 0 idle_after → 10.
func withDefaults(s *mycelv1.SharingSettings) *mycelv1.SharingSettings {
	if s == nil {
		return Default(Offers)
	}
	out := &mycelv1.SharingSettings{
		Offer:            &mycelv1.Capabilities{Names: offerSet(s)},
		When:             whenOf(s),
		Schedule:         s.GetSchedule(),
		LocalFirst:       s.GetLocalFirst(),
		MaxJobs:          s.GetMaxJobs(),
		IdleAfterMinutes: s.GetIdleAfterMinutes(),
	}
	if out.MaxJobs <= 0 {
		out.MaxJobs = DefaultMaxJobs
	}
	if out.IdleAfterMinutes <= 0 {
		out.IdleAfterMinutes = DefaultIdleAfter
	}
	return out
}

// offerSet returns the known offers s grants, in Offers order; a nil s or a
// nil Offer grants every known offer.
func offerSet(s *mycelv1.SharingSettings) []string {
	if s.GetOffer() == nil {
		return slices.Clone(Offers)
	}
	return intersectOffers(Offers, s.GetOffer().GetNames())
}

// intersectOffers returns the elements of a that are in b, in a's order.
func intersectOffers(a, b []string) []string {
	out := []string{}
	for _, o := range a {
		if slices.Contains(b, o) {
			out = append(out, o)
		}
	}
	return out
}

func whenOf(s *mycelv1.SharingSettings) string {
	if s.GetWhen() == "" {
		return WhenAlways
	}
	return s.GetWhen()
}
