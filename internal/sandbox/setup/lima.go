package setup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/profiles"
)

// SetupProfile is the profile whose image setup pre-warms and boots once.
const SetupProfile = "default"

// imageCacher is the macOS backend's cache probe (applevf.Backend).
type imageCacher interface {
	ImageCached(image string) (cached bool, location string, err error)
}

// Lima prepares the macOS backend (Lima, Apple Virtualization.framework).
type Lima struct {
	Options
	Backend  sandbox.Backend
	LimaHome string // the backend's LIMA_HOME, for `limactl list`
	LookPath func(string) (string, error)
	// NewID returns a sandbox id for the test VM (random sbx:<ULID> when nil).
	NewID func() string
}

const limaInstall = "install Lima with `brew install lima` (https://lima-vm.io), then run `mycel sandbox setup` again"

func (l *Lima) image() (string, *sandbox.Limits, error) {
	p, err := profiles.Get(SetupProfile)
	if err != nil {
		return "", nil, err
	}
	lim, err := sandbox.ResolveLimits(p.GetLimits(), nil)
	if err != nil {
		return "", nil, err
	}
	return p.GetImage(), &lim, nil
}

// Setup implements Platform.
func (l *Lima) Setup(ctx context.Context) error {
	l.defaults()
	path, err := l.LookPath("limactl")
	if err != nil {
		return &PrereqError{Missing: "Lima (limactl) is not installed", Instruction: limaInstall}
	}
	if out, err := l.output(ctx, []string{path, "--version"}); err == nil {
		l.logf("found %s", firstLine(string(out)))
	}
	if l.Backend == nil {
		return errors.New("this binary has no macOS sandbox backend")
	}
	r := probe(ctx, l.Backend, "darwin")
	if !r.Available {
		return fmt.Errorf("the %s backend is not available: %v (upgrade with `brew upgrade lima`)", r.Backend, r.Details["reason"])
	}
	image, limits, err := l.image()
	if err != nil {
		return err
	}
	if c, ok := l.Backend.(imageCacher); ok {
		cached, loc, err := c.ImageCached(image)
		if err != nil {
			return err
		}
		if !cached {
			if err := l.consent("the Linux VM image for "+image+" ("+filepath.Base(loc)+")", 400); err != nil {
				return err
			}
		}
	}
	l.logf("preparing the VM image for %s", image)
	if err := l.Backend.Prepare(ctx, image, func(s string) { l.logf("  %s", s) }); err != nil {
		return fmt.Errorf("prepare %s: %w", image, err)
	}
	return l.bootOnce(ctx, image, *limits)
}

// bootOnce starts one throwaway sandbox VM from the profile, runs `true` in
// it and destroys it, so Lima's first boot (and any failure) happens now.
func (l *Lima) bootOnce(ctx context.Context, image string, limits sandbox.Limits) error {
	p, err := profiles.Get(SetupProfile)
	if err != nil {
		return err
	}
	id := newSandboxID()
	if l.NewID != nil {
		id = l.NewID()
	}
	// One directory (and VM) per run, so an interrupted run never collides
	// with the next one.
	dir := filepath.Join(l.Dir, "setup-vm-"+strings.ToLower(strings.TrimPrefix(id, "sbx:")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	limits.WallClock = 15 * time.Minute
	l.logf("starting a test VM (the first boot takes a few minutes)")
	start := time.Now()
	sb, err := l.Backend.Create(ctx, sandbox.Spec{ID: id, Profile: p, Image: image, Limits: limits, Dir: dir})
	if err != nil {
		return fmt.Errorf("start test VM: %w", err)
	}
	defer func() {
		if err := sb.Destroy(context.WithoutCancel(ctx)); err != nil {
			l.logf("removing the test VM failed: %v", err)
		}
	}()
	proc, err := sb.Exec(ctx, []string{"true"}, sandbox.ExecOpts{})
	if err != nil {
		return fmt.Errorf("run in test VM: %w", err)
	}
	var wg sync.WaitGroup
	for _, r := range []io.Reader{proc.Stdout(), proc.Stderr()} {
		if r == nil {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, r) }()
	}
	code, err := proc.Wait()
	wg.Wait()
	if err != nil {
		return fmt.Errorf("run in test VM: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("run in test VM: exit code %d", code)
	}
	l.logf("test VM ran a command in %s; the sandbox backend is ready", time.Since(start).Round(time.Second))
	return nil
}

// Status implements Platform.
func (l *Lima) Status(ctx context.Context) Report {
	l.defaults()
	r := probe(ctx, l.Backend, "darwin")
	d := r.Details
	path, err := l.LookPath("limactl")
	if err != nil {
		d["lima"] = ""
		d["hint"] = limaInstall
		return r
	}
	if out, err := l.output(ctx, []string{path, "--version"}); err == nil {
		f := strings.Fields(firstLine(string(out)))
		if len(f) > 0 {
			d["lima"] = strings.TrimPrefix(f[len(f)-1], "v")
		}
	}
	if image, _, err := l.image(); err == nil {
		d["image"] = image
		if c, ok := l.Backend.(imageCacher); ok {
			cached, loc, err := c.ImageCached(image)
			if err == nil {
				d["image_cached"] = cached
				d["image_url"] = loc
			}
		}
	}
	if l.LimaHome != "" {
		env := []string{"LIMA_HOME=" + l.LimaHome}
		for _, k := range []string{"HOME", "PATH", "TMPDIR"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
		var out strings.Builder
		if err := l.Runner.Run(ctx, Cmd{Argv: []string{path, "list", "--format", "{{.Name}} {{.Status}}"}, Env: env, Stdout: &out}); err == nil {
			total, running := 0, 0
			for _, line := range strings.Split(out.String(), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 || !strings.HasPrefix(f[0], "sbx-") {
					continue
				}
				total++
				if f[1] == "Running" {
					running++
				}
			}
			d["vms"] = total
			d["vms_running"] = running
		}
	}
	if !r.Available {
		d["hint"] = "run `mycel sandbox setup`"
	}
	return r
}

// newSandboxID returns sbx:<26 Crockford base32 characters> (ULID shaped).
func newSandboxID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [26]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	for i := 9; i >= 0; i-- {
		b[i] = alphabet[ms&31]
		ms >>= 5
	}
	for i := 10; i < 26; i++ {
		b[i] = alphabet[b[i]&31]
	}
	return "sbx:" + string(b[:])
}
