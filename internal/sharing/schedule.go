package sharing

import (
	"errors"
	"fmt"
	"slices"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Days are the schedule day names, Monday first.
var Days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

const (
	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
)

// span is a window on one day: to in 1..1440, or < from for overnight.
type span struct{ day, from, to int }

// week is a set of minutes of the week, minute 0 being Monday 00:00 local.
type week [minutesPerWeek]bool

// minuteOfWeek returns t's wall-clock minute of the week in t's location.
// Wall-clock evaluation keeps DST transitions harmless: a skipped hour is
// simply never seen and a repeated hour is inside or outside the window both
// times.
func minuteOfWeek(t time.Time) int {
	day := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return day*minutesPerDay + t.Hour()*60 + t.Minute()
}

// parseHHMM parses "HH:MM" into minutes after midnight; allow24 admits
// "24:00" (end of day, valid only as a window's end).
func parseHHMM(v string, allow24 bool) (int, error) {
	if len(v) != 5 || v[2] != ':' {
		return 0, fmt.Errorf("time %q must be HH:MM", v)
	}
	digit := func(c byte) (int, bool) { return int(c - '0'), c >= '0' && c <= '9' }
	h1, ok1 := digit(v[0])
	h2, ok2 := digit(v[1])
	m1, ok3 := digit(v[3])
	m2, ok4 := digit(v[4])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return 0, fmt.Errorf("time %q must be HH:MM", v)
	}
	h, m := h1*10+h2, m1*10+m2
	if allow24 && h == 24 && m == 0 {
		return minutesPerDay, nil
	}
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("time %q out of range 00:00..23:59", v)
	}
	return h*60 + m, nil
}

func formatHHMM(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func validateWindow(w *mycelv1.ScheduleWindow) error {
	if w == nil {
		return errors.New("empty window")
	}
	if len(w.GetDays()) == 0 {
		return errors.New("days must name at least one of mon..sun")
	}
	seen := map[string]bool{}
	for _, d := range w.GetDays() {
		if !slices.Contains(Days, d) {
			return fmt.Errorf("unknown day %q (want mon..sun)", d)
		}
		if seen[d] {
			return fmt.Errorf("duplicate day %q", d)
		}
		seen[d] = true
	}
	from, err := parseHHMM(w.GetFrom(), false)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	to, err := parseHHMM(w.GetTo(), true)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if from == to {
		return fmt.Errorf("from and to are both %s", w.GetFrom())
	}
	return nil
}

// weekOf returns the minutes covered by windows. Invalid windows (which
// Validate rejects) contribute nothing.
func weekOf(windows []*mycelv1.ScheduleWindow) *week {
	var wk week
	for _, w := range windows {
		if validateWindow(w) != nil {
			continue
		}
		from, _ := parseHHMM(w.GetFrom(), false)
		to, _ := parseHHMM(w.GetTo(), true)
		length := to - from
		if length < 0 { // overnight
			length += minutesPerDay
		}
		for _, d := range w.GetDays() {
			start := slices.Index(Days, d)*minutesPerDay + from
			for i := range length {
				wk[(start+i)%minutesPerWeek] = true
			}
		}
	}
	return &wk
}

// inWindows reports whether minute-of-week m lies in one of windows; it
// agrees with weekOf(windows)[m] without building the set.
func inWindows(windows []*mycelv1.ScheduleWindow, m int) bool {
	for _, w := range windows {
		if validateWindow(w) != nil {
			continue
		}
		from, _ := parseHHMM(w.GetFrom(), false)
		to, _ := parseHHMM(w.GetTo(), true)
		length := to - from
		if length < 0 {
			length += minutesPerDay
		}
		for _, d := range w.GetDays() {
			start := slices.Index(Days, d)*minutesPerDay + from
			if (m-start+minutesPerWeek)%minutesPerWeek < length {
				return true
			}
		}
	}
	return false
}

func (w *week) empty() bool { return !slices.Contains(w[:], true) }

func (w *week) and(o *week) *week {
	var out week
	for i := range out {
		out[i] = w[i] && o[i]
	}
	return &out
}

func (w *week) subsetOf(o *week) bool {
	for i := range w {
		if w[i] && !o[i] {
			return false
		}
	}
	return true
}

// windows returns the normalised windows covering exactly w: maximal runs,
// a run crossing one midnight as a single overnight window when it is
// shorter than a day, longer runs split per day ("to" "24:00" for a run
// reaching midnight), identical day-times grouped into one window, sorted by
// first day then from.
func (w *week) windows() []*mycelv1.ScheduleWindow {
	var spans []span
	start := -1
	for i := range minutesPerWeek {
		if !w[i] {
			start = i
			break
		}
	}
	if start < 0 { // the whole week
		for d := range 7 {
			spans = append(spans, span{d, 0, minutesPerDay})
		}
	} else {
		// Walk the circular week from a free minute so no run is cut.
		for i := 1; i <= minutesPerWeek; {
			m := (start + i) % minutesPerWeek
			if !w[m] {
				i++
				continue
			}
			n := 0
			for i+n <= minutesPerWeek && w[(start+i+n)%minutesPerWeek] {
				n++
			}
			spans = append(spans, splitRun(m, n)...)
			i += n
		}
	}
	type key struct{ from, to int }
	groups := map[key][]int{}
	var order []key
	for _, s := range spans {
		k := key{s.from, s.to}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s.day)
	}
	out := make([]*mycelv1.ScheduleWindow, 0, len(order))
	for _, k := range order {
		ds := groups[k]
		slices.Sort(ds)
		ds = slices.Compact(ds)
		names := make([]string, len(ds))
		for i, d := range ds {
			names[i] = Days[d]
		}
		to := "24:00"
		if k.to < minutesPerDay {
			to = formatHHMM(k.to)
		}
		out = append(out, &mycelv1.ScheduleWindow{Days: names, From: formatHHMM(k.from), To: to})
	}
	slices.SortStableFunc(out, func(a, b *mycelv1.ScheduleWindow) int {
		da, db := slices.Index(Days, a.GetDays()[0]), slices.Index(Days, b.GetDays()[0])
		if da != db {
			return da - db
		}
		fa, _ := parseHHMM(a.GetFrom(), false)
		fb, _ := parseHHMM(b.GetFrom(), false)
		return fa - fb
	})
	return out
}

// splitRun splits the run of n minutes starting at minute-of-week m into
// day spans.
func splitRun(m, n int) []span {
	day, from := m/minutesPerDay, m%minutesPerDay
	if from+n > minutesPerDay && n < minutesPerDay {
		// Shorter than a day and crosses one midnight: one overnight window.
		return []span{{day, from, (from + n) % minutesPerDay}}
	}
	var out []span
	for n > 0 {
		l := min(n, minutesPerDay-from)
		out = append(out, span{day, from, from + l})
		n -= l
		day, from = (day+1)%7, 0
	}
	return out
}
