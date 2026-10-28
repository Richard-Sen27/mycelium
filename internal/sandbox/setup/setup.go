// Package setup provisions and checks the sandbox backend of this machine
// (`mycel sandbox setup` and `mycel sandbox status`):
//
//   - Windows (WSL): checks WSL 2, imports the dedicated distro mycel-sbx from
//     a pinned Alpine minimal root filesystem, installs the packages the
//     wsl2-gvisor backend runs (bundle.DistroRequirements), places the
//     pinned gVisor release in /usr/local/bin, writes /etc/wsl.conf so the
//     distro sees no Windows drives and cannot start Windows programs, and
//     verifies the result. Every step checks first, so a re-run repairs.
//   - macOS (Lima): checks limactl, warms Lima's cache with the pinned VM
//     image through the backend's Prepare and boots one throwaway sandbox VM
//     so the first job does not wait for the first boot.
//   - Linux (gVisor): checks runsc (and with --yes installs the pinned
//     release into ~/.local/bin), cgroup v2 and user namespaces.
//
// Status reports every platform as {backend, available, details}, where
// available and its reason come from the backend's own Available probe.
//
// Every external program runs through a Runner and every download through a
// Downloader, so the logic of all three platforms is unit tested on any OS.
// Downloads are pinned by URL and digest (sources.go), verified before use
// and removed when they do not match.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/gvisor/bundle"
)

// Platform is one machine's provisioning.
type Platform interface {
	// Setup checks, installs and repairs what the backend needs. A missing
	// manual prerequisite is a *PrereqError.
	Setup(ctx context.Context) error
	// Status reports the backend's state without changing anything.
	Status(ctx context.Context) Report
}

// Report is the result of Status (`mycel sandbox status --json`).
type Report struct {
	Backend   string         `json:"backend"`
	Available bool           `json:"available"`
	Details   map[string]any `json:"details"`
}

// PrereqError is a prerequisite the user has to install by hand
// (`mycel sandbox setup` exits 2).
type PrereqError struct {
	Missing     string // what is missing
	Instruction string // what to run
}

func (e *PrereqError) Error() string { return e.Missing + ": " + e.Instruction }

// Errors.
var (
	// ErrChecksum is a download whose digest does not match the pinned one.
	ErrChecksum = errors.New("setup: checksum mismatch")
	// ErrNeedsConsent is a download that needs --yes or an interactive yes.
	ErrNeedsConsent = errors.New("setup: download not confirmed")
)

// Cmd is one external command; there is no shell on the host side.
type Cmd struct {
	Argv   []string
	Env    []string // nil: inherit the environment
	Stdin  io.Reader
	Stdout io.Writer
}

// Runner runs external commands. Tests use a recording fake.
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// Downloader fetches a URL into w, failing when the body exceeds max bytes.
// Platform tests use a fake; nothing in this package's tests touches the
// network (HTTPDownloader is tested against a local TLS server).
type Downloader interface {
	Get(ctx context.Context, url string, max int64, w io.Writer) error
}

// Options are shared by every platform.
type Options struct {
	Runner     Runner     // ExecRunner{} when nil
	Downloader Downloader // HTTPDownloader{} when nil
	// Dir holds downloads (and the WSL distro's disk on Windows).
	Dir string
	// Yes agrees to the downloads without asking.
	Yes bool
	// Confirm asks the user a yes/no question; nil when nobody can answer
	// (then a download without Yes fails with ErrNeedsConsent).
	Confirm func(question string) (bool, error)
	// Log receives the progress log, one line per call.
	Log func(line string)
	// Home is the user's home directory, shortened to ~ in the log.
	Home string
}

func (o *Options) defaults() {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Downloader == nil {
		o.Downloader = HTTPDownloader{}
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
}

func (o *Options) logf(format string, args ...any) {
	o.Log(o.tidy(fmt.Sprintf(format, args...)))
}

// tidy replaces the home directory with ~ so logs carry no user names.
func (o *Options) tidy(s string) string {
	if o.Home == "" || len(o.Home) < 4 {
		return s
	}
	return strings.ReplaceAll(s, o.Home, "~")
}

func (o *Options) output(ctx context.Context, argv []string) ([]byte, error) {
	var out bytes.Buffer
	err := o.Runner.Run(ctx, Cmd{Argv: argv, Stdout: &out})
	return out.Bytes(), err
}

// ExecRunner runs commands with os/exec. A failure carries the first 4 KiB
// of stderr.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Cmd) error {
	if len(c.Argv) == 0 {
		return errors.New("setup: empty argv")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) // #nosec G204 -- argv built by this package, never a shell
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderr, n: 4096}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(bundle.DecodeWSLText(stderr.Bytes()))
		name := filepath.Base(c.Argv[0])
		if len(c.Argv) > 1 {
			name += " " + c.Argv[1]
		}
		if msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		_, _ = l.w.Write(q)
		l.n -= len(q)
	}
	return len(p), nil
}

// BackendName is the backend `mycel sandbox setup` provisions on goos.
func BackendName(goos string) string {
	switch goos {
	case "darwin":
		return sandbox.BackendAppleVF
	case "windows":
		return sandbox.BackendWSL2GVisor
	case "linux":
		return sandbox.BackendGVisor
	}
	return ""
}

// HostBackend is the registered backend for this OS, or nil when the binary
// has none (tests, unsupported OS).
func HostBackend() sandbox.Backend {
	name := BackendName(runtime.GOOS)
	for _, b := range sandbox.Backends() {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

// HostConfig is what ForHost needs beyond Options.
type HostConfig struct {
	Backend  sandbox.Backend // the platform backend (nil: none compiled in)
	LimaHome string          // macOS: the backend's LIMA_HOME
	BinDir   string          // Linux: where --yes installs runsc (~/.local/bin)
}

// ForHost returns the Platform of this OS.
func ForHost(o Options, hc HostConfig) (Platform, error) {
	switch runtime.GOOS {
	case "windows":
		return &WSL{Options: o, Backend: hc.Backend}, nil
	case "darwin":
		return &Lima{Options: o, Backend: hc.Backend, LimaHome: hc.LimaHome, LookPath: exec.LookPath}, nil
	case "linux":
		return &Linux{Options: o, Backend: hc.Backend, BinDir: hc.BinDir, LookPath: exec.LookPath,
			ReadFile: os.ReadFile, GOARCH: runtime.GOARCH}, nil
	}
	return nil, fmt.Errorf("setup: no sandbox backend for %s", runtime.GOOS)
}

// probe runs the backend's own Available check.
func probe(ctx context.Context, b sandbox.Backend, goos string) Report {
	r := Report{Backend: BackendName(goos), Details: map[string]any{}}
	if b == nil {
		r.Details["reason"] = "no sandbox backend is compiled into this binary"
		return r
	}
	r.Backend = b.Name()
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, why := b.Available(pctx)
	r.Available = ok
	if !ok {
		r.Details["reason"] = why
	}
	return r
}

// consent asks once for the downloads that are not cached yet.
func (o *Options) consent(what string, mb int64) error {
	if o.Yes {
		return nil
	}
	q := fmt.Sprintf("Download %s (about %d MB)?", what, mb)
	if o.Confirm == nil {
		return fmt.Errorf("%w: %s needs about %d MB of downloads; run `mycel sandbox setup --yes` to agree", ErrNeedsConsent, what, mb)
	}
	ok, err := o.Confirm(q)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: declined", ErrNeedsConsent)
	}
	return nil
}
