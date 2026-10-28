package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sandbox/setup"
)

// sandboxBackend is the platform backend (setup.HostBackend); tests replace
// it.
var sandboxBackend = setup.HostBackend

// newSandboxPlatform builds the provisioning for this OS; tests replace it.
var newSandboxPlatform = setup.ForHost

const sandboxSetupHint = "run `mycel sandbox setup`"

func (a *app) sandboxCmd() *cobra.Command {
	cmd := groupCmd("sandbox", "Set up and check this machine's sandbox backend (WSL2 + gVisor, Lima, gVisor)")
	cmd.AddCommand(a.sandboxSetupCmd(), a.sandboxStatusCmd())
	return cmd
}

// sandboxPlatform resolves the defaults: <data dir>/sandbox for downloads
// (and the WSL distro), <node dir>/lima as LIMA_HOME, ~/.local/bin for a
// Linux runsc.
func (a *app) sandboxPlatform(dir string, yes bool, log func(string)) (setup.Platform, error) {
	dirs, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = filepath.Join(dirs.Data, "sandbox")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	o := setup.Options{Dir: dir, Yes: yes, Log: log, Home: home}
	if a.isTTY() {
		in := bufio.NewReader(a.stdin)
		o.Confirm = func(q string) (bool, error) {
			_, _ = fmt.Fprintf(a.stderr, "%s [y/N] ", q)
			line, err := in.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return false, err
			}
			ans := strings.ToLower(strings.TrimSpace(line))
			return ans == "y" || ans == "yes", nil
		}
	}
	hc := setup.HostConfig{Backend: sandboxBackend(), LimaHome: filepath.Join(dirs.NodeDir(), "lima")}
	if home != "" {
		hc.BinDir = filepath.Join(home, ".local", "bin")
	}
	return newSandboxPlatform(o, hc)
}

func (a *app) sandboxSetupCmd() *cobra.Command {
	var yes bool
	var dir string
	cmd := &cobra.Command{
		Use:   "setup [--yes] [--dir d]",
		Short: "Install or repair the sandbox backend: the mycel-sbx WSL distro on Windows, Lima's VM image on macOS, gVisor checks on Linux",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logTo := a.stdout
			if a.jsonOut {
				logTo = a.stderr
			}
			p, err := a.sandboxPlatform(dir, yes, func(line string) { _, _ = fmt.Fprintln(logTo, line) })
			if err != nil {
				return err
			}
			// Ctrl-C cancels setup; the Lima test VM is still destroyed.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := p.Setup(ctx); err != nil {
				var pe *setup.PrereqError
				if errors.As(err, &pe) {
					return exitCodeError{code: exitUsage, err: err}
				}
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(p.Status(ctx))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "agree to the downloads without asking")
	cmd.Flags().StringVar(&dir, "dir", "", "directory for downloads and the WSL distro's disk (default <data dir>/sandbox)")
	return cmd
}

func (a *app) sandboxStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether this machine's sandbox backend is ready",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.sandboxPlatform("", false, nil)
			if err != nil {
				return err
			}
			r := p.Status(cmd.Context())
			if a.jsonOut {
				return a.printJSONValue(r)
			}
			yes := "no"
			if r.Available {
				yes = "yes"
			}
			tw := newTable(a.stdout)
			row(tw, "backend:", r.Backend)
			row(tw, "available:", yes)
			keys := make([]string, 0, len(r.Details))
			for k := range r.Details {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				row(tw, k+":", dash(fmt.Sprint(r.Details[k])))
			}
			return tw.Flush()
		},
	}
}

// platformBackendIssue returns why the platform backend is unavailable, or
// "" when it is available or none is compiled in.
func platformBackendIssue(ctx context.Context, timeout time.Duration) string {
	b := sandboxBackend()
	if b == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if ok, why := b.Available(ctx); !ok {
		if why == "" {
			why = sandbox.ErrUnavailable.Error()
		}
		return why
	}
	return ""
}

// sandboxHint logs a warning when the platform backend is unavailable (the
// daemon's startup; runs in the background, probe bounded at 30 s).
func (a *app) sandboxHint(ctx context.Context) {
	if why := platformBackendIssue(ctx, 30*time.Second); why != "" {
		a.log.Warn("sandbox backend unavailable; jobs cannot run on this node: "+sandboxSetupHint, "reason", why)
	}
}

// printSandboxHint prints one line after `mycel join` when the platform
// backend is unavailable (probe bounded at 10 s).
func (a *app) printSandboxHint(ctx context.Context, w io.Writer) {
	if why := platformBackendIssue(ctx, 10*time.Second); why != "" {
		_, _ = fmt.Fprintf(w, "\nSandboxes are not ready on this machine (%s): %s.\n", why, sandboxSetupHint)
	}
}
