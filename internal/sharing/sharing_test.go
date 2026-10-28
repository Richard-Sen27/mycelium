package sharing

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func caps(names ...string) *mycelv1.Capabilities { return &mycelv1.Capabilities{Names: names} }

func win(days, from, to string) *mycelv1.ScheduleWindow {
	return &mycelv1.ScheduleWindow{Days: strings.Split(days, ","), From: from, To: to}
}

func ws(w ...*mycelv1.ScheduleWindow) []*mycelv1.ScheduleWindow { return w }

func sched(w ...*mycelv1.ScheduleWindow) *mycelv1.SharingSettings {
	return &mycelv1.SharingSettings{When: WhenSchedule, Schedule: w}
}

func idleIn(after int32, w ...*mycelv1.ScheduleWindow) *mycelv1.SharingSettings {
	return &mycelv1.SharingSettings{When: WhenIdle, IdleAfterMinutes: after, Schedule: w}
}

// setLocal makes loc the process-local zone for the test.
func setLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// at returns the given wall-clock time in loc (2026-10-26 is a Monday).
func at(loc *time.Location, day, hh, mm int) time.Time {
	return time.Date(2026, 10, 26+day, hh, mm, 0, 0, loc)
}

func TestDefault(t *testing.T) {
	tests := []struct {
		ceiling []string
		want    []string
	}{
		{nil, []string{}},
		{[]string{"gpu", "docker"}, []string{"docker", "gpu"}},
		{[]string{"sandbox", "bogus", "ollama", "gpu", "docker"}, []string{"docker", "ollama", "gpu", "sandbox"}},
	}
	for _, tt := range tests {
		got := Default(tt.ceiling)
		want := &mycelv1.SharingSettings{Offer: caps(tt.want...), When: "always", LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}
		if !proto.Equal(got, want) {
			t.Errorf("Default(%v) = %v, want %v", tt.ceiling, got, want)
		}
		if err := Validate(got); err != nil {
			t.Errorf("Validate(Default(%v)): %v", tt.ceiling, err)
		}
	}
}

func TestValidate(t *testing.T) {
	windows := func(n int) []*mycelv1.ScheduleWindow {
		var out []*mycelv1.ScheduleWindow
		for range n {
			out = append(out, win("mon", "08:00", "09:00"))
		}
		return out
	}
	tests := []struct {
		name string
		s    *mycelv1.SharingSettings
		err  string
	}{
		{"nil", nil, ""},
		{"empty", &mycelv1.SharingSettings{}, ""},
		{"all offers", &mycelv1.SharingSettings{Offer: caps(Offers...)}, ""},
		{"no offers", &mycelv1.SharingSettings{Offer: caps()}, ""},
		{"unknown offer", &mycelv1.SharingSettings{Offer: caps("docker", "tpu")}, "unknown offer"},
		{"duplicate offer", &mycelv1.SharingSettings{Offer: caps("gpu", "gpu")}, "duplicate offer"},
		{"when always", &mycelv1.SharingSettings{When: "always"}, ""},
		{"when idle", &mycelv1.SharingSettings{When: "idle"}, ""},
		{"when bogus", &mycelv1.SharingSettings{When: "never"}, "when"},
		{"schedule", sched(win("mon,tue,wed,thu,fri", "09:00", "17:00")), ""},
		{"schedule without windows", sched(), "at least one window"},
		{"idle with windows", idleIn(0, win("sat", "00:00", "24:00")), ""},
		{"always with windows", &mycelv1.SharingSettings{When: "always", Schedule: ws(win("mon", "08:00", "09:00"))}, "need when"},
		{"empty when with windows", &mycelv1.SharingSettings{Schedule: ws(win("mon", "08:00", "09:00"))}, "need when"},
		{"overnight", sched(win("fri,sat", "22:00", "07:00")), ""},
		{"to midnight", sched(win("sun", "18:00", "00:00")), ""},
		{"to 24:00", sched(win("sun", "00:00", "24:00")), ""},
		{"from 24:00", sched(win("sun", "24:00", "07:00")), "from"},
		{"from == to", sched(win("mon", "08:00", "08:00")), "both"},
		{"bad day", sched(win("monday", "08:00", "09:00")), "unknown day"},
		{"dup day", sched(win("mon,mon", "08:00", "09:00")), "duplicate day"},
		{"no days", sched(&mycelv1.ScheduleWindow{From: "08:00", To: "09:00"}), "at least one of"},
		{"nil window", sched(nil), "empty window"},
		{"bad hour", sched(win("mon", "25:00", "09:00")), "out of range"},
		{"bad minute", sched(win("mon", "08:60", "09:00")), "out of range"},
		{"short", sched(win("mon", "8:00", "09:00")), "HH:MM"},
		{"no colon", sched(win("mon", "08.00", "09:00")), "HH:MM"},
		{"letters", sched(win("mon", "ab:cd", "09:00")), "HH:MM"},
		{"32 windows", sched(windows(32)...), ""},
		{"33 windows", sched(windows(33)...), "at most 32"},
		{"max_jobs 64", &mycelv1.SharingSettings{MaxJobs: 64}, ""},
		{"max_jobs 65", &mycelv1.SharingSettings{MaxJobs: 65}, "max_jobs"},
		{"max_jobs -1", &mycelv1.SharingSettings{MaxJobs: -1}, "max_jobs"},
		{"idle 1440", &mycelv1.SharingSettings{IdleAfterMinutes: 1440}, ""},
		{"idle 1441", &mycelv1.SharingSettings{IdleAfterMinutes: 1441}, "idle_after"},
		{"idle -1", &mycelv1.SharingSettings{IdleAfterMinutes: -1}, "idle_after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.s)
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.err != "" && err == nil:
				t.Fatalf("want error containing %q", tt.err)
			case tt.err != "" && !strings.Contains(err.Error(), tt.err):
				t.Fatalf("error %q does not contain %q", err, tt.err)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	full := Default(Offers)
	all := caps(Offers...)
	never := &mycelv1.SharingSettings{Offer: caps(), When: "schedule", LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}
	tests := []struct {
		name       string
		hub, local *mycelv1.SharingSettings
		want       *mycelv1.SharingSettings
	}{
		{"nil nil", nil, nil, full},
		{"nil local is defaulted hub", &mycelv1.SharingSettings{Offer: caps("gpu")}, nil,
			&mycelv1.SharingSettings{Offer: caps("gpu"), When: "always", MaxJobs: 2, IdleAfterMinutes: 10}},
		{"nil hub", nil, &mycelv1.SharingSettings{Offer: caps("ollama")},
			&mycelv1.SharingSettings{Offer: caps("ollama"), When: "always", LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}},
		{"offers intersect", &mycelv1.SharingSettings{Offer: caps("docker", "gpu", "ollama")}, &mycelv1.SharingSettings{Offer: caps("sandbox", "gpu", "docker")},
			&mycelv1.SharingSettings{Offer: caps("docker", "gpu"), When: "always", MaxJobs: 2, IdleAfterMinutes: 10}},
		{"partial local keeps hub offers", full, &mycelv1.SharingSettings{MaxJobs: 1},
			&mycelv1.SharingSettings{Offer: all, When: "always", LocalFirst: true, MaxJobs: 1, IdleAfterMinutes: 10}},
		{"local offers nothing", full, &mycelv1.SharingSettings{Offer: caps()},
			&mycelv1.SharingSettings{Offer: caps(), When: "always", LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}},
		{"max_jobs min non-zero", &mycelv1.SharingSettings{MaxJobs: 8}, &mycelv1.SharingSettings{MaxJobs: 3},
			&mycelv1.SharingSettings{Offer: all, When: "always", MaxJobs: 3, IdleAfterMinutes: 10}},
		{"local max_jobs larger ignored", &mycelv1.SharingSettings{MaxJobs: 4}, &mycelv1.SharingSettings{MaxJobs: 9},
			&mycelv1.SharingSettings{Offer: all, When: "always", MaxJobs: 4, IdleAfterMinutes: 10}},
		{"local_first either", &mycelv1.SharingSettings{}, &mycelv1.SharingSettings{LocalFirst: true},
			&mycelv1.SharingSettings{Offer: all, When: "always", LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}},
		{"idle and always", &mycelv1.SharingSettings{When: "always"}, idleIn(5),
			&mycelv1.SharingSettings{Offer: all, When: "idle", MaxJobs: 2, IdleAfterMinutes: 10}},
		{"idle keeps larger idle_after", idleIn(15), idleIn(30),
			&mycelv1.SharingSettings{Offer: all, When: "idle", MaxJobs: 2, IdleAfterMinutes: 30}},
		{"hub schedule and local idle", sched(win("mon", "09:00", "17:00")), idleIn(0),
			&mycelv1.SharingSettings{Offer: all, When: "idle", Schedule: ws(win("mon", "09:00", "17:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"hub idle and local schedule", idleIn(20), sched(win("sat", "22:00", "07:00")),
			&mycelv1.SharingSettings{Offer: all, When: "idle", Schedule: ws(win("sat", "22:00", "07:00")), MaxJobs: 2, IdleAfterMinutes: 20}},
		{"idle windows intersect", idleIn(0, win("mon,tue", "08:00", "18:00")), idleIn(0, win("tue,wed", "12:00", "20:00")),
			&mycelv1.SharingSettings{Offer: all, When: "idle", Schedule: ws(win("tue", "12:00", "18:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"idle with disjoint windows is never", idleIn(0, win("mon", "08:00", "10:00")), sched(win("tue", "08:00", "10:00")),
			&mycelv1.SharingSettings{Offer: all, When: "schedule", MaxJobs: 2, IdleAfterMinutes: 10}},
		{"schedule and always (hub)", sched(win("tue,mon", "09:00", "17:00")), &mycelv1.SharingSettings{When: "always"},
			&mycelv1.SharingSettings{Offer: all, When: "schedule", Schedule: ws(win("mon,tue", "09:00", "17:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"schedule and always (local)", nil, sched(win("sat", "22:00", "07:00")),
			&mycelv1.SharingSettings{Offer: all, When: "schedule", Schedule: ws(win("sat", "22:00", "07:00")), LocalFirst: true, MaxJobs: 2, IdleAfterMinutes: 10}},
		{"schedules intersect", sched(win("mon,tue,wed,thu,fri", "08:00", "18:00")), sched(win("mon,wed", "12:00", "20:00"), win("sun", "00:00", "24:00")),
			&mycelv1.SharingSettings{Offer: all, When: "schedule", Schedule: ws(win("mon,wed", "12:00", "18:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"overnight intersect", sched(win("fri", "20:00", "08:00")), sched(win("fri,sat", "23:00", "06:00")),
			&mycelv1.SharingSettings{Offer: all, When: "schedule", Schedule: ws(win("fri", "23:00", "06:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"disjoint schedules", sched(win("mon", "08:00", "10:00")), sched(win("mon", "10:00", "12:00")),
			&mycelv1.SharingSettings{Offer: all, When: "schedule", MaxJobs: 2, IdleAfterMinutes: 10}},
		{"windows merged", sched(win("mon", "08:00", "10:00"), win("mon", "09:00", "12:00"), win("tue", "08:00", "12:00")), nil,
			&mycelv1.SharingSettings{Offer: all, When: "schedule", Schedule: ws(win("mon,tue", "08:00", "12:00")), MaxJobs: 2, IdleAfterMinutes: 10}},
		{"invalid hub", &mycelv1.SharingSettings{When: "sometimes"}, nil, never},
		{"invalid local", full, &mycelv1.SharingSettings{Offer: caps("tpu")}, never},
		{"unknown local when", full, &mycelv1.SharingSettings{When: "weekends"}, never},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Effective(tt.hub, tt.local)
			if !proto.Equal(got, tt.want) {
				t.Fatalf("Effective =\n %v\nwant\n %v", got, tt.want)
			}
			if got.GetWhen() != WhenSchedule || len(got.GetSchedule()) > 0 {
				if err := Validate(got); err != nil {
					t.Fatalf("Validate(Effective): %v", err)
				}
			}
			if !Tightens(tt.hub, got) {
				t.Errorf("Effective is wider than the hub settings")
			}
		})
	}
}

func TestWeekWindows(t *testing.T) {
	tests := []struct {
		name string
		in   []*mycelv1.ScheduleWindow
		want []*mycelv1.ScheduleWindow
	}{
		{"empty", nil, ws()},
		{"whole week", ws(win("mon,tue,wed,thu,fri,sat,sun", "00:00", "24:00")), ws(win("mon,tue,wed,thu,fri,sat,sun", "00:00", "24:00"))},
		{"sunday overnight wraps", ws(win("sun", "22:00", "06:00")), ws(win("sun", "22:00", "06:00"))},
		{"to midnight is end of day", ws(win("mon", "18:00", "00:00")), ws(win("mon", "18:00", "24:00"))},
		{"adjacent days join overnight", ws(win("mon", "20:00", "24:00"), win("tue", "00:00", "04:00")), ws(win("mon", "20:00", "04:00"))},
		{"long run split per day", ws(win("mon", "20:00", "24:00"), win("tue", "00:00", "24:00"), win("wed", "00:00", "04:00")),
			ws(win("mon", "20:00", "24:00"), win("tue", "00:00", "24:00"), win("wed", "00:00", "04:00"))},
		{"grouped by time", ws(win("fri", "22:00", "07:00"), win("mon", "22:00", "07:00"), win("mon", "12:00", "13:00")),
			ws(win("mon", "12:00", "13:00"), win("mon,fri", "22:00", "07:00"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := weekOf(tt.in).windows()
			if len(got) != len(tt.want) {
				t.Fatalf("windows = %v, want %v", got, tt.want)
			}
			for i := range got {
				if !proto.Equal(got[i], tt.want[i]) {
					t.Fatalf("windows = %v, want %v", got, tt.want)
				}
			}
			if *weekOf(got) != *weekOf(tt.in) {
				t.Fatalf("normalised windows cover different minutes")
			}
			wk := weekOf(tt.in)
			for m := range minutesPerWeek {
				if inWindows(tt.in, m) != wk[m] {
					t.Fatalf("inWindows(%d) disagrees with weekOf", m)
				}
			}
		})
	}
}

func TestTightens(t *testing.T) {
	base := &mycelv1.SharingSettings{Offer: caps("docker", "gpu"), When: "schedule", Schedule: ws(win("fri,sat", "22:00", "07:00")), LocalFirst: true, MaxJobs: 4, IdleAfterMinutes: 10}
	with := func(b *mycelv1.SharingSettings, f func(s *mycelv1.SharingSettings)) *mycelv1.SharingSettings {
		s := proto.Clone(b).(*mycelv1.SharingSettings)
		f(s)
		return s
	}
	cand := func(f func(s *mycelv1.SharingSettings)) *mycelv1.SharingSettings { return with(base, f) }
	hub := Default(Offers)
	tests := []struct {
		name      string
		base, cnd *mycelv1.SharingSettings
		want      bool
	}{
		{"equal", base, base, true},
		{"fewer offers", base, cand(func(s *mycelv1.SharingSettings) { s.Offer = caps("gpu") }), true},
		{"no offers", base, cand(func(s *mycelv1.SharingSettings) { s.Offer = caps() }), true},
		{"extra offer", base, cand(func(s *mycelv1.SharingSettings) { s.Offer = caps("gpu", "ollama") }), false},
		{"nil offer is everything", base, cand(func(s *mycelv1.SharingSettings) { s.Offer = nil }), false},
		{"schedule to always", base, cand(func(s *mycelv1.SharingSettings) { s.When, s.Schedule = "always", nil }), false},
		{"schedule to empty when", base, cand(func(s *mycelv1.SharingSettings) { s.When, s.Schedule = "", nil }), false},
		{"schedule to idle anywhere", base, cand(func(s *mycelv1.SharingSettings) { s.When, s.Schedule = "idle", nil }), false},
		{"schedule to idle in windows", base, cand(func(s *mycelv1.SharingSettings) { s.When = "idle" }), true},
		{"narrower window", base, cand(func(s *mycelv1.SharingSettings) { s.Schedule = ws(win("sat", "23:00", "06:00")) }), true},
		{"window across sunday morning", base, cand(func(s *mycelv1.SharingSettings) { s.Schedule = ws(win("sun", "01:00", "07:00")) }), true},
		{"wider window", base, cand(func(s *mycelv1.SharingSettings) { s.Schedule = ws(win("sat", "21:00", "07:00")) }), false},
		{"other day", base, cand(func(s *mycelv1.SharingSettings) { s.Schedule = ws(win("mon", "23:00", "06:00")) }), false},
		{"schedule without windows is never", base, cand(func(s *mycelv1.SharingSettings) { s.Schedule = nil }), true},
		{"local_first off", base, cand(func(s *mycelv1.SharingSettings) { s.LocalFirst = false }), false},
		{"local_first on", cand(func(s *mycelv1.SharingSettings) { s.LocalFirst = false }), base, true},
		{"fewer jobs", base, cand(func(s *mycelv1.SharingSettings) { s.MaxJobs = 1 }), true},
		{"more jobs", base, cand(func(s *mycelv1.SharingSettings) { s.MaxJobs = 5 }), false},
		{"jobs 0 is no limit", base, cand(func(s *mycelv1.SharingSettings) { s.MaxJobs = 0 }), false},
		{"base jobs 0 is no limit", cand(func(s *mycelv1.SharingSettings) { s.MaxJobs = 0 }), cand(func(s *mycelv1.SharingSettings) { s.MaxJobs = 30 }), true},
		{"longer idle_after", base, cand(func(s *mycelv1.SharingSettings) { s.IdleAfterMinutes = 30 }), true},
		{"shorter idle_after", base, cand(func(s *mycelv1.SharingSettings) { s.IdleAfterMinutes = 5 }), false},
		{"idle_after 0 is no limit", base, cand(func(s *mycelv1.SharingSettings) { s.IdleAfterMinutes = 0 }), false},
		{"always to schedule", &mycelv1.SharingSettings{}, sched(win("mon", "08:00", "09:00")), true},
		{"always to idle", &mycelv1.SharingSettings{}, idleIn(0), true},
		{"idle to schedule", idleIn(0), sched(win("mon", "08:00", "09:00")), false},
		{"idle to always", idleIn(0), &mycelv1.SharingSettings{}, false},
		{"idle to idle in windows", idleIn(0), idleIn(0, win("mon", "08:00", "09:00")), true},
		{"idle in windows to idle anywhere", idleIn(0, win("mon", "08:00", "09:00")), idleIn(0), false},
		{"idle in windows to narrower", idleIn(0, win("mon", "08:00", "09:00")), idleIn(0, win("mon", "08:15", "08:45")), true},
		{"unknown candidate when is strictest", base, cand(func(s *mycelv1.SharingSettings) { s.When = "weekends" }), true},
		{"unknown base when", cand(func(s *mycelv1.SharingSettings) { s.When = "weekends" }), base, false},
		{"both unknown", cand(func(s *mycelv1.SharingSettings) { s.When = "x" }), cand(func(s *mycelv1.SharingSettings) { s.When = "y" }), true},
		{"pure tightening, rest unset", &mycelv1.SharingSettings{Offer: caps("docker", "gpu")}, &mycelv1.SharingSettings{Offer: caps("gpu")}, true},
		{"pure tightening of a hub", Effective(hub, nil), Effective(hub, &mycelv1.SharingSettings{Offer: caps("gpu")}), true},
		{"nil base, nil cand", nil, nil, true},
		{"nil base", nil, &mycelv1.SharingSettings{Offer: caps("gpu")}, true},
		{"nil base, empty cand", nil, &mycelv1.SharingSettings{}, true},
		{"nil cand is no ceiling", base, nil, false},
		{"empty base, nil cand", &mycelv1.SharingSettings{}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Tightens(tt.base, tt.cnd); got != tt.want {
				t.Fatalf("Tightens = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestActiveAt(t *testing.T) {
	setLocal(t, time.UTC)
	utc := time.UTC
	idle := idleIn(15)
	night := sched(win("fri,sat", "22:00", "07:00"))
	idleNight := idleIn(0, win("fri,sat", "22:00", "07:00"))
	week := sched(win("sun", "23:00", "01:00"))
	tests := []struct {
		name string
		s    *mycelv1.SharingSettings
		t    time.Time
		idle time.Duration
		want bool
	}{
		{"nil", nil, at(utc, 0, 12, 0), 0, true},
		{"always", &mycelv1.SharingSettings{When: "always"}, at(utc, 0, 12, 0), 0, true},
		{"empty when", &mycelv1.SharingSettings{}, at(utc, 0, 12, 0), 0, true},
		{"unknown when", &mycelv1.SharingSettings{When: "weekends"}, at(utc, 5, 12, 0), 48 * time.Hour, false},
		{"idle not yet", idle, at(utc, 0, 12, 0), 14*time.Minute + 59*time.Second, false},
		{"idle exactly", idle, at(utc, 0, 12, 0), 15 * time.Minute, true},
		{"idle default 10", idleIn(0), at(utc, 0, 12, 0), 10 * time.Minute, true},
		{"idle default 10, 9m", idleIn(0), at(utc, 0, 12, 0), 9 * time.Minute, false},
		{"idle in window, idle", idleNight, at(utc, 4, 23, 0), time.Hour, true},
		{"idle in window, active", idleNight, at(utc, 4, 23, 0), 0, false},
		{"idle outside window", idleNight, at(utc, 4, 12, 0), time.Hour, false},
		{"fri 21:59", night, at(utc, 4, 21, 59), 0, false},
		{"fri 22:00", night, at(utc, 4, 22, 0), 0, true},
		{"sat 03:00", night, at(utc, 5, 3, 0), 0, true},
		{"sat 06:59", night, at(utc, 5, 6, 59), 0, true},
		{"sat 07:00", night, at(utc, 5, 7, 0), 0, false},
		{"sun 06:00", night, at(utc, 6, 6, 0), 0, true},
		{"mon 06:00", night, at(utc, 7, 6, 0), 0, false},
		{"thu 23:00", night, at(utc, 3, 23, 0), 0, false},
		{"sun 23:30 wraps", week, at(utc, 6, 23, 30), 0, true},
		{"mon 00:30 wraps", week, at(utc, 7, 0, 30), 0, true},
		{"mon 01:00 wraps", week, at(utc, 7, 1, 0), 0, false},
		{"empty schedule never", sched(), at(utc, 0, 12, 0), time.Hour, false},
		{"schedule ignores idle", night, at(utc, 0, 12, 0), 24 * time.Hour, false},
		// 21:30 UTC is 22:30 in UTC+1, but ActiveAt uses time.Local (UTC).
		{"evaluated in time.Local", night, at(utc, 4, 21, 30).In(time.FixedZone("UTC+1", 3600)), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActiveAt(tt.s, tt.t, tt.idle); got != tt.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}
}

func TestActiveAtDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	setLocal(t, berlin)
	// 2026-03-29 (Sunday) 02:00 CET jumps to 03:00 CEST; 2026-10-25 (Sunday)
	// 03:00 CEST falls back to 02:00 CET. Times are given in UTC; ActiveAt
	// converts them to time.Local.
	s := sched(win("sat", "23:00", "02:30"), win("sun", "03:00", "04:00"))
	utc := func(mo time.Month, d, hh, mm int) time.Time { return time.Date(2026, mo, d, hh, mm, 0, 0, time.UTC) }
	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"spring 00:59 UTC is 01:59 CET", utc(3, 29, 0, 59), true},
		{"spring 01:00 UTC is 03:00 CEST", utc(3, 29, 1, 0), true},
		{"spring 01:15 UTC is 03:15 CEST", utc(3, 29, 1, 15), true},
		{"spring 02:00 UTC is 04:00 CEST", utc(3, 29, 2, 0), false},
		{"fall 00:15 UTC is first 02:15 CEST", utc(10, 25, 0, 15), true},
		{"fall 01:15 UTC is second 02:15 CET", utc(10, 25, 1, 15), true},
		{"fall 00:45 UTC is first 02:45 CEST", utc(10, 25, 0, 45), false},
		{"fall 01:45 UTC is second 02:45 CET", utc(10, 25, 1, 45), false},
		{"fall 02:30 UTC is 03:30 CET", utc(10, 25, 2, 30), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActiveAt(s, tt.t, 0); got != tt.want {
				t.Fatalf("ActiveAt(%s) = %v, want %v", tt.t.In(berlin), got, tt.want)
			}
		})
	}
}

func TestOfferOrderStable(t *testing.T) {
	got := Effective(&mycelv1.SharingSettings{Offer: caps("sandbox", "docker")}, nil).GetOffer().GetNames()
	if !slices.Equal(got, []string{"docker", "sandbox"}) {
		t.Fatalf("offers = %v", got)
	}
}

// ---- property tests ----

func newRand() *rand.Rand { return rand.New(rand.NewPCG(5, 24)) } // #nosec G404 -- deterministic test data

func randWindow(r *rand.Rand) *mycelv1.ScheduleWindow {
	var days []string
	for _, d := range Days {
		if r.IntN(3) == 0 {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		days = []string{Days[r.IntN(7)]}
	}
	from := r.IntN(96) * 15
	to := from
	for to == from {
		to = (r.IntN(96) + 1) * 15 // 00:15..24:00
	}
	toS := "24:00"
	if to < minutesPerDay {
		toS = formatHHMM(to)
	}
	return &mycelv1.ScheduleWindow{Days: days, From: formatHHMM(from), To: toS}
}

// randSettings returns valid settings (or nil), including zero fields.
func randSettings(r *rand.Rand) *mycelv1.SharingSettings {
	if r.IntN(8) == 0 {
		return nil
	}
	s := &mycelv1.SharingSettings{
		LocalFirst:       r.IntN(2) == 0,
		MaxJobs:          []int32{0, 0, 1, 2, 3, 5}[r.IntN(6)],
		IdleAfterMinutes: []int32{0, 0, 5, 10, 20}[r.IntN(5)],
	}
	if r.IntN(3) > 0 {
		s.Offer = caps()
		for _, o := range Offers {
			if r.IntN(2) == 0 {
				s.Offer.Names = append(s.Offer.Names, o)
			}
		}
	}
	switch r.IntN(4) {
	case 0:
	case 1:
		s.When = WhenAlways
	case 2:
		s.When = WhenSchedule
		for range 1 + r.IntN(3) {
			s.Schedule = append(s.Schedule, randWindow(r))
		}
	case 3:
		s.When = WhenIdle
		for range r.IntN(3) {
			s.Schedule = append(s.Schedule, randWindow(r))
		}
	}
	if err := Validate(s); err != nil {
		panic(err)
	}
	return s
}

func describe(s *mycelv1.SharingSettings) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprint(s)
}

// TestEffectiveIsConjunction checks minute by minute, owner idle and
// active, that Effective is active exactly when both inputs are.
func TestEffectiveIsConjunction(t *testing.T) {
	setLocal(t, time.UTC)
	r := newRand()
	monday := at(time.UTC, 0, 0, 0)
	for i := range 60 {
		hub, local := randSettings(r), randSettings(r)
		eff := Effective(hub, local)
		if len(eff.GetSchedule()) > MaxWindows {
			t.Errorf("case %d: %d windows", i, len(eff.GetSchedule()))
		}
		for _, idle := range []time.Duration{0, 25 * time.Hour} {
			for m := range minutesPerWeek {
				ts := monday.Add(time.Duration(m) * time.Minute)
				want := ActiveAt(withDefaults(hub), ts, idle) && ActiveAt(local, ts, idle)
				if got := ActiveAt(eff, ts, idle); got != want {
					t.Fatalf("case %d, minute %d, idle %v: Effective active %v, want %v\nhub   %s\nlocal %s\neff   %s",
						i, m, idle, got, want, describe(hub), describe(local), describe(eff))
				}
			}
		}
	}
}

func TestTightensProperties(t *testing.T) {
	r := newRand()
	for i := range 2000 {
		a, x, y := randSettings(r), randSettings(r), randSettings(r)
		if !Tightens(a, a) {
			t.Fatalf("case %d: not reflexive: %s", i, describe(a))
		}
		b := Effective(a, x)
		if !Tightens(a, b) {
			t.Fatalf("case %d: Tightens(hub, Effective(hub, local)) false\nhub   %s\nlocal %s\neff   %s", i, describe(a), describe(x), describe(b))
		}
		// Transitivity over derived and independent triples.
		c := Effective(b, y)
		triples := [][3]*mycelv1.SharingSettings{{a, b, c}, {a, x, y}, {x, a, b}, {a, y, c}}
		for j, tr := range triples {
			if Tightens(tr[0], tr[1]) && Tightens(tr[1], tr[2]) && !Tightens(tr[0], tr[2]) {
				t.Fatalf("case %d/%d: not transitive\n%s\n%s\n%s", i, j, describe(tr[0]), describe(tr[1]), describe(tr[2]))
			}
		}
	}
}

// TestTightensMeansNarrowerEffective checks the definition: when Tightens
// holds, Effective(H, candidate) is within Effective(H, base) for any H.
func TestTightensMeansNarrowerEffective(t *testing.T) {
	setLocal(t, time.UTC)
	r := newRand()
	monday := at(time.UTC, 0, 0, 0)
	checked := 0
	for checked < 40 {
		base, h := randSettings(r), randSettings(r)
		cand := Effective(base, randSettings(r))
		if r.IntN(2) == 0 {
			cand = randSettings(r)
		}
		if !Tightens(base, cand) {
			continue
		}
		checked++
		eb, ec := Effective(h, base), Effective(h, cand)
		for _, o := range ec.GetOffer().GetNames() {
			if !slices.Contains(eb.GetOffer().GetNames(), o) {
				t.Fatalf("offer %s widened", o)
			}
		}
		if ec.GetMaxJobs() > eb.GetMaxJobs() || ec.GetIdleAfterMinutes() < eb.GetIdleAfterMinutes() || (eb.GetLocalFirst() && !ec.GetLocalFirst()) {
			t.Fatalf("limits widened\nbase %s\ncand %s\nhub  %s", describe(base), describe(cand), describe(h))
		}
		for _, idle := range []time.Duration{0, 25 * time.Hour} {
			for m := 0; m < minutesPerWeek; m += 5 {
				ts := monday.Add(time.Duration(m) * time.Minute)
				if ActiveAt(ec, ts, idle) && !ActiveAt(eb, ts, idle) {
					t.Fatalf("minute %d idle %v: candidate active, base not\nbase %s\ncand %s\nhub  %s", m, idle, describe(base), describe(cand), describe(h))
				}
			}
		}
	}
}
