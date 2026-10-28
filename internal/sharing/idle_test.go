package sharing

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const ioregFixture = `+-o Root  <class IORegistryEntry, id 0x100000100, retain 32>
  +-o J314sAP  <class IOPlatformExpertDevice, id 0x100000226, registered, matched, active, busy 0 (1290 ms), retain 39>
    +-o AppleARMPE  <class AppleARMPE, id 0x100000227, registered, matched, active, busy 0 (1265 ms), retain 22>
      +-o IOHIDSystem  <class IOHIDSystem, id 0x1000004c5, registered, matched, active, busy 0 (0 ms), retain 26>
          {
            "IOClass" = "IOHIDSystem"
            "HIDParameters" = {"HIDClickTime"=500000000,"HIDKeyRepeat"=83333333}
            "HIDIdleTime" = 754312458
            "IOProviderClass" = "IOResources"
          }
`

func TestParseIoreg(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want time.Duration
		err  string
	}{
		{"fixture", ioregFixture, 754312458 * time.Nanosecond, ""},
		{"long idle", `    |   "HIDIdleTime" = 3600000000000`, time.Hour, ""},
		{"first wins", "\"HIDIdleTime\" = 1000\n\"HIDIdleTime\" = 2000\n", time.Microsecond, ""},
		{"missing", `"IOClass" = "IOHIDSystem"`, 0, "no HIDIdleTime"},
		{"garbage", `"HIDIdleTime" = <data>`, 0, "HIDIdleTime"},
		{"empty", "", 0, "no HIDIdleTime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIoreg([]byte(tt.out))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("parseIoreg = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestIoregDetector(t *testing.T) {
	var gotArgs []string
	fixture := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > IdleTimeout {
			t.Errorf("call not capped at %v", IdleTimeout)
		}
		gotArgs = append([]string{name}, args...)
		return []byte(ioregFixture), nil
	}
	sbin := func(p string) bool { return p == "/usr/sbin/ioreg" }
	none := func(string) bool { return false }
	onPath := func(n string) (string, error) { return "/opt/bin/" + n, nil }
	noPath := func(string) (string, error) { return "", errors.New("not found") }

	d := ioregDetector{run: fixture, exists: sbin, lookPath: onPath}
	got, err := d.IdleFor()
	if err != nil || got != 754312458*time.Nanosecond {
		t.Fatalf("IdleFor = %v, %v", got, err)
	}
	if strings.Join(gotArgs, " ") != "/usr/sbin/ioreg -c IOHIDSystem -d 4" {
		t.Fatalf("ran %v", gotArgs)
	}
	d.exists = none
	if _, err := d.IdleFor(); err != nil || gotArgs[0] != "/opt/bin/ioreg" {
		t.Fatalf("PATH fallback ran %v, %v", gotArgs, err)
	}

	// Every failure means "not idle" with a nil error.
	failures := map[string]ioregDetector{
		"missing tool":  {run: fixture, exists: none, lookPath: noPath},
		"command fails": {run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("boom") }, exists: sbin, lookPath: noPath},
		"bad output":    {run: func(context.Context, string, ...string) ([]byte, error) { return []byte("nothing"), nil }, exists: sbin, lookPath: noPath},
	}
	for name, d := range failures {
		if got, err := d.IdleFor(); got != 0 || err != nil {
			t.Errorf("%s: IdleFor = %v, %v; want 0, nil", name, got, err)
		}
	}
}

func TestExecRun(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	out, err := execRun(ctx, sh, "-c", "echo hello")
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("execRun = %q, %v", out, err)
	}
	if _, err := execRun(ctx, sh, "-c", "head -c 1048577 /dev/zero"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized output: %v", err)
	}
	if out, err := execRun(ctx, sh, "-c", "head -c 1048576 /dev/zero"); err != nil || len(out) != maxOutput {
		t.Fatalf("output at the cap: %d, %v", len(out), err)
	}
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The grandchild keeps stdout open; WaitDelay bounds the wait.
	if _, err := execRun(tctx, sh, "-c", "sleep 5 & sleep 5"); err == nil {
		t.Fatal("want error after the deadline")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("execRun took %v after the deadline", el)
	}
}

func TestParseLoginctl(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	since := now.Add(-7 * time.Minute).UnixMicro()
	tests := []struct {
		name string
		out  string
		want time.Duration
		err  bool
	}{
		{"idle", "IdleHint=yes\nIdleSinceHint=" + itoa(since) + "\n", 7 * time.Minute, false},
		{"active", "IdleHint=no\nIdleSinceHint=" + itoa(since) + "\n", 0, false},
		{"active, zero since", "IdleHint=no\nIdleSinceHint=0\n", 0, false},
		{"future since", "IdleHint=yes\nIdleSinceHint=" + itoa(now.Add(time.Minute).UnixMicro()) + "\n", 0, false},
		{"idle, no since", "IdleHint=yes\n", 0, true},
		{"idle, zero since", "IdleHint=yes\nIdleSinceHint=0\n", 0, true},
		{"no hint", "IdleSinceHint=5\n", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLoginctl([]byte(tt.out), now)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("parseLoginctl = %v, %v; want %v, err %v", got, err, tt.want, tt.err)
			}
		})
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

type fakeCmds struct {
	have map[string]bool
	out  map[string]string
	err  map[string]error
	ran  []string
}

func (f *fakeCmds) exists(path string) bool {
	return f.have[strings.TrimPrefix(path, "/usr/bin/")]
}

func (f *fakeCmds) lookPath(name string) (string, error) {
	if f.have[name] {
		return "/usr/local/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeCmds) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("no deadline")
	}
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.ran = append(f.ran, cmd)
	if err := f.err[cmd]; err != nil {
		return nil, err
	}
	return []byte(f.out[cmd]), nil
}

func TestLinuxDetector(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const seat = "/usr/bin/loginctl show-seat seat0 -p ActiveSession --value"
	const sess = "/usr/bin/loginctl show-session 3 -p IdleHint -p IdleSinceHint"
	const xp = "/usr/local/bin/xprintidle"
	idleOut := "IdleHint=yes\nIdleSinceHint=" + itoa(now.Add(-20*time.Minute).UnixMicro()) + "\n"
	tests := []struct {
		name string
		f    fakeCmds
		want time.Duration
	}{
		{"logind idle", fakeCmds{have: map[string]bool{"loginctl": true, "xprintidle": true}, out: map[string]string{seat: "3\n", sess: idleOut, xp: "1000"}}, 20 * time.Minute},
		{"logind active", fakeCmds{have: map[string]bool{"loginctl": true, "xprintidle": true}, out: map[string]string{seat: "3\n", sess: "IdleHint=no\nIdleSinceHint=0\n", xp: "999999"}}, 0},
		{"no active session falls back", fakeCmds{have: map[string]bool{"loginctl": true, "xprintidle": true}, out: map[string]string{seat: "\n", xp: "90000\n"}}, 90 * time.Second},
		{"loginctl fails falls back", fakeCmds{have: map[string]bool{"loginctl": true, "xprintidle": true}, err: map[string]error{seat: errors.New("no seat")}, out: map[string]string{xp: "5000"}}, 5 * time.Second},
		{"xprintidle only", fakeCmds{have: map[string]bool{"xprintidle": true}, out: map[string]string{xp: "60000"}}, time.Minute},
		{"xprintidle garbage", fakeCmds{have: map[string]bool{"xprintidle": true}, out: map[string]string{xp: "couldn't open display"}}, 0},
		{"dash session id rejected", fakeCmds{have: map[string]bool{"loginctl": true}, out: map[string]string{seat: "--help\n", "/usr/bin/loginctl show-session --help -p IdleHint -p IdleSinceHint": idleOut}}, 0},
		{"nothing", fakeCmds{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f
			d := linuxDetector{run: f.run, exists: f.exists, lookPath: f.lookPath, now: func() time.Time { return now }}
			got, err := d.IdleFor()
			if err != nil || got != tt.want {
				t.Fatalf("IdleFor = %v, %v; want %v (ran %v)", got, err, tt.want, f.ran)
			}
			for _, cmd := range f.ran {
				if strings.Contains(cmd, "show-session -") {
					t.Fatalf("ran %q with a dash session id", cmd)
				}
			}
		})
	}
}

func TestNewIdleDetector(t *testing.T) {
	d := NewIdleDetector()
	if d == nil {
		t.Fatal("nil detector")
	}
	start := time.Now()
	idle, err := d.IdleFor()
	if err == nil && idle < 0 {
		t.Fatalf("negative idle %v", idle)
	}
	if el := time.Since(start); el > IdleTimeout+time.Second {
		t.Fatalf("IdleFor took %v", el)
	}
	if v, err := (nopDetector{}).IdleFor(); v != 0 || err != nil {
		t.Fatalf("nop = %v, %v", v, err)
	}
	_ = newLinuxDetector()
	_ = newIoregDetector()
}
