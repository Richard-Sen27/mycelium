package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/setup"
)

// The cli tests run without a platform backend (the darwin test binary has
// applevf registered): the join and daemon hints stay silent unless a test
// sets one.
func init() { sandboxBackend = func() sandbox.Backend { return nil } }

type fakePlatform struct {
	opts     setup.Options
	hc       setup.HostConfig
	setupErr error
	ran      bool
}

func (p *fakePlatform) Setup(context.Context) error {
	p.ran = true
	p.opts.Log("step one")
	return p.setupErr
}

func (p *fakePlatform) Status(context.Context) setup.Report {
	return setup.Report{Backend: "wsl2-gvisor", Available: p.setupErr == nil, Details: map[string]any{"distro": "mycel-sbx"}}
}

func useFakePlatform(t *testing.T, setupErr error) *fakePlatform {
	t.Helper()
	t.Setenv(config.EnvHome, t.TempDir())
	p := &fakePlatform{setupErr: setupErr}
	old := newSandboxPlatform
	newSandboxPlatform = func(o setup.Options, hc setup.HostConfig) (setup.Platform, error) {
		p.opts, p.hc = o, hc
		return p, nil
	}
	t.Cleanup(func() { newSandboxPlatform = old })
	return p
}

type hintBackend struct {
	sandbox.Backend
	ok bool
}

func (b hintBackend) Name() string { return "wsl2-gvisor" }
func (b hintBackend) Available(context.Context) (bool, string) {
	if b.ok {
		return true, ""
	}
	return false, "WSL distro mycel-sbx is not installed"
}

func useHintBackend(t *testing.T, b sandbox.Backend) {
	t.Helper()
	old := sandboxBackend
	sandboxBackend = func() sandbox.Backend { return b }
	t.Cleanup(func() { sandboxBackend = old })
}

func TestSandboxSetup(t *testing.T) {
	p := useFakePlatform(t, nil)
	res := runCLI(t, "", "sandbox", "setup", "--yes", "--dir", "sbx")
	if res.code != 0 || !strings.Contains(res.stdout, "step one") {
		t.Fatalf("%+v", res)
	}
	abs, _ := filepath.Abs("sbx")
	if !p.ran || !p.opts.Yes || p.opts.Dir != abs || p.opts.Confirm != nil {
		t.Fatalf("options %+v", p.opts)
	}
	if !strings.HasSuffix(p.hc.LimaHome, filepath.Join("node", "lima")) {
		t.Fatalf("lima home %q", p.hc.LimaHome)
	}

	// --json: progress on stderr, the status on stdout.
	res = runCLI(t, "", "sandbox", "setup", "--json")
	var rep setup.Report
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &rep) != nil || rep.Backend != "wsl2-gvisor" || !strings.Contains(res.stderr, "step one") {
		t.Fatalf("%+v", res)
	}
	d := p.opts.Dir
	if want := filepath.Join(must(config.Resolve()).Data, "sandbox"); d != want {
		t.Fatalf("default dir %q, want %q", d, want)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestSandboxSetupExitCodes(t *testing.T) {
	useFakePlatform(t, &setup.PrereqError{Missing: "WSL 2 is not installed", Instruction: "run `wsl --install`"})
	res := runCLI(t, "", "sandbox", "setup")
	if res.code != 2 || !strings.Contains(res.stderr, "wsl --install") {
		t.Fatalf("%+v", res)
	}
	useFakePlatform(t, errors.New("apk add: exit status 1"))
	if res := runCLI(t, "", "sandbox", "setup"); res.code != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestSandboxStatus(t *testing.T) {
	useFakePlatform(t, nil)
	res := runCLI(t, "", "sandbox", "status", "--json")
	var got map[string]any
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &got) != nil {
		t.Fatalf("%+v", res)
	}
	if got["backend"] != "wsl2-gvisor" || got["available"] != true || got["details"].(map[string]any)["distro"] != "mycel-sbx" {
		t.Fatalf("status %v", got)
	}
	res = runCLI(t, "", "sandbox", "status")
	if !strings.Contains(res.stdout, "available:  yes") || !strings.Contains(res.stdout, "distro:     mycel-sbx") {
		t.Fatalf("text %q", res.stdout)
	}
}

func TestJoinPrintsSandboxHint(t *testing.T) {
	for _, ok := range []bool{false, true} {
		t.Setenv(config.EnvHome, t.TempDir())
		useHintBackend(t, hintBackend{ok: ok})
		ta := newTestApp(t, "")
		ta.daemonJoin = func(context.Context, daemon.JoinOptions) (*daemon.JoinResult, error) {
			return &daemon.JoinResult{NodeName: "n1", HubURL: "https://hub.test:7777"}, nil
		}
		res := ta.run("join", "tok")
		if res.code != 0 {
			t.Fatalf("%+v", res)
		}
		has := strings.Contains(res.stdout, "run `mycel sandbox setup`")
		if has == ok {
			t.Fatalf("available=%v: hint shown=%v:\n%s", ok, has, res.stdout)
		}
	}
}
