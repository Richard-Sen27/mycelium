package sharing

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// IdleTimeout caps a single IdleFor call.
const IdleTimeout = 2 * time.Second

const (
	waitDelay = 500 * time.Millisecond // after the deadline, for the output pipes
	maxOutput = 1 << 20                // stdout cap per command
)

// runFunc runs a command and returns its standard output.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// execRun runs a command with stdout capped at maxOutput.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed tool paths and arguments
	cmd.WaitDelay = waitDelay
	out := &capWriter{max: maxOutput}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.over {
		return nil, fmt.Errorf("%s: output exceeds %d bytes", name, maxOutput)
	}
	return out.buf.Bytes(), nil
}

// capWriter keeps the first max bytes and drops the rest.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) > room {
		w.over = true
		w.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return w.buf.Write(p)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// resolveTool prefers the fixed system path and falls back to PATH; "" means
// the tool is unavailable.
func resolveTool(exists func(string) bool, lookPath func(string) (string, error), fixed, name string) string {
	if fixed != "" && exists(fixed) {
		return fixed
	}
	if p, err := lookPath(name); err == nil {
		return p
	}
	return ""
}

func warnNotIdle(msg string, args ...any) {
	slog.Default().Warn("sharing: idle detection: "+msg+"; treating the owner as active", args...)
}

// nopDetector cannot tell and reports 0 (never idle).
type nopDetector struct{}

func (nopDetector) IdleFor() (time.Duration, error) { return 0, nil }

// ioregDetector reads the macOS HID idle time from ioreg. Failures are
// logged and reported as 0 (never idle).
type ioregDetector struct {
	run      runFunc
	exists   func(string) bool
	lookPath func(string) (string, error)
}

func newIoregDetector() ioregDetector {
	return ioregDetector{run: execRun, exists: fileExists, lookPath: exec.LookPath}
}

func (d ioregDetector) IdleFor() (time.Duration, error) {
	tool := resolveTool(d.exists, d.lookPath, "/usr/sbin/ioreg", "ioreg")
	if tool == "" {
		warnNotIdle("ioreg not found")
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), IdleTimeout)
	defer cancel()
	out, err := d.run(ctx, tool, "-c", "IOHIDSystem", "-d", "4")
	if err != nil {
		warnNotIdle("ioreg failed", "err", err)
		return 0, nil
	}
	idle, err := parseIoreg(out)
	if err != nil {
		warnNotIdle("ioreg output", "err", err)
		return 0, nil
	}
	return idle, nil
}

// parseIoreg extracts the first "HIDIdleTime" = <nanoseconds> from ioreg
// output.
func parseIoreg(out []byte) (time.Duration, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), maxOutput)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, `"HIDIdleTime"`)
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len(`"HIDIdleTime"`):])
		rest, ok := strings.CutPrefix(rest, "=")
		if !ok {
			continue
		}
		ns, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 63)
		if err != nil {
			return 0, fmt.Errorf("sharing: ioreg HIDIdleTime %q: %w", strings.TrimSpace(rest), err)
		}
		return time.Duration(ns), nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("sharing: ioreg output: %w", err)
	}
	return 0, errors.New("sharing: ioreg output has no HIDIdleTime")
}

// linuxDetector asks logind for the active session's idle hint, falls back
// to xprintidle and otherwise reports 0 (never idle).
type linuxDetector struct {
	run      runFunc
	exists   func(string) bool
	lookPath func(string) (string, error)
	now      func() time.Time
}

func newLinuxDetector() linuxDetector {
	return linuxDetector{run: execRun, exists: fileExists, lookPath: exec.LookPath, now: time.Now}
}

func (d linuxDetector) IdleFor() (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), IdleTimeout)
	defer cancel()
	if tool := resolveTool(d.exists, d.lookPath, "/usr/bin/loginctl", "loginctl"); tool != "" {
		if idle, ok := d.logind(ctx, tool); ok {
			return idle, nil
		}
	}
	if tool := resolveTool(d.exists, d.lookPath, "", "xprintidle"); tool != "" {
		out, err := d.run(ctx, tool)
		if err == nil {
			ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err == nil && ms >= 0 {
				return time.Duration(ms) * time.Millisecond, nil
			}
		}
	}
	return 0, nil
}

// logind returns the idle time of seat0's active session; ok is false when
// logind cannot tell (no seat, no active session, unparsable output).
func (d linuxDetector) logind(ctx context.Context, tool string) (time.Duration, bool) {
	out, err := d.run(ctx, tool, "show-seat", "seat0", "-p", "ActiveSession", "--value")
	if err != nil {
		return 0, false
	}
	session := strings.TrimSpace(string(out))
	if session == "" || strings.HasPrefix(session, "-") || strings.ContainsAny(session, " \t\n") {
		return 0, false
	}
	out, err = d.run(ctx, tool, "show-session", session, "-p", "IdleHint", "-p", "IdleSinceHint")
	if err != nil {
		return 0, false
	}
	idle, err := parseLoginctl(out, d.now())
	if err != nil {
		return 0, false
	}
	return idle, true
}

// parseLoginctl parses `loginctl show-session -p IdleHint -p IdleSinceHint`
// output: IdleSinceHint is microseconds since the epoch at which the session
// went idle; IdleHint=no means the owner is active.
func parseLoginctl(out []byte, now time.Time) (time.Duration, error) {
	props := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			props[k] = v
		}
	}
	hint, ok := props["IdleHint"]
	if !ok {
		return 0, errors.New("sharing: loginctl output has no IdleHint")
	}
	if hint != "yes" {
		return 0, nil
	}
	since, err := strconv.ParseInt(props["IdleSinceHint"], 10, 64)
	if err != nil || since <= 0 {
		return 0, fmt.Errorf("sharing: loginctl IdleSinceHint %q", props["IdleSinceHint"])
	}
	idle := now.Sub(time.UnixMicro(since))
	if idle < 0 {
		return 0, nil
	}
	return idle, nil
}
