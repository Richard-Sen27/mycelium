package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/service"
)

// newServiceManager and redirectStderr are the service package's; tests
// replace them.
var (
	newServiceManager = service.New
	redirectStderr    = service.RedirectStderr
)

type serviceStatusJSON struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Path      string `json:"path"`
	LogFile   string `json:"log_file"`
}

func (a *app) serviceCmd() *cobra.Command {
	cmd := groupCmd("service", "Keep the daemon running in the background for this user (launchd, systemd --user, Task Scheduler)")
	cmd.AddCommand(a.serviceInstallCmd(), a.serviceUninstallCmd(), a.serviceStartCmd(), a.serviceStopCmd(),
		a.serviceStatusCmd(), a.serviceLogsCmd())
	return cmd
}

func (a *app) serviceManager() (service.Manager, string, error) {
	home := os.Getenv(config.EnvHome)
	m, err := newServiceManager(home)
	if err != nil {
		return nil, "", err
	}
	logFile, err := service.LogFile(home)
	if err != nil {
		return nil, "", err
	}
	return m, logFile, nil
}

func (a *app) serviceInstallCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "install [--role node|hub]",
		Short: "Install and start the daemon as a per-user background service",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			hasHub := fileExists(filepath.Join(dirs.HubDir(), "hub.yaml"))
			hasNode := fileExists(filepath.Join(dirs.NodeDir(), "node.yaml"))
			switch role {
			case "":
				role = service.RoleNode
				if hasHub {
					role = service.RoleHub
				}
			case service.RoleNode, service.RoleHub:
			default:
				return usagef("--role must be node or hub, not %q", role)
			}
			if role == service.RoleHub && !hasHub {
				return errors.New("the hub role is not initialised here: run `mycel hub init --advertise <url>` first")
			}
			if !hasHub && !hasNode {
				return errors.New("nothing to run yet: `mycel join <token>` (or `mycel hub init`) first")
			}
			m, logFile, err := a.serviceManager()
			if err != nil {
				return err
			}
			if err := m.Install(role); err != nil {
				return fmt.Errorf("service install: %w", err)
			}
			st, err := m.Status()
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(statusJSON(st, logFile))
			}
			_, _ = fmt.Fprintf(a.stdout, "Installed %s (role %s); the daemon starts now and at every login.\nLogs: %s (`mycel service logs -f`)\n", st.Path, role, logFile)
			if n, ok := m.(service.Noter); ok {
				for _, line := range n.Notes() {
					_, _ = fmt.Fprintln(a.stdout, line)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "node or hub (default: hub if this machine runs the hub, else node); both run `mycel daemon`")
	return cmd
}

func statusJSON(st service.Status, logFile string) serviceStatusJSON {
	return serviceStatusJSON{Installed: st.Installed, Running: st.Running, PID: st.PID, Path: st.Path, LogFile: logFile}
}

// serviceActionCmd is uninstall, start or stop.
func (a *app) serviceActionCmd(use, short, done string, act func(service.Manager) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			m, _, err := a.serviceManager()
			if err != nil {
				return err
			}
			if err := act(m); err != nil {
				if use != "uninstall" || !errors.Is(err, service.ErrNotInstalled) {
					return fmt.Errorf("service %s: %w", use, err)
				}
				done = "Not installed; nothing to do."
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]bool{"ok": true})
			}
			_, err = fmt.Fprintln(a.stdout, done)
			return err
		},
	}
}

func (a *app) serviceUninstallCmd() *cobra.Command {
	return a.serviceActionCmd("uninstall", "Stop the daemon and remove the background service (keeps data and logs)",
		"Uninstalled; data and logs are kept.", service.Manager.Uninstall)
}

func (a *app) serviceStartCmd() *cobra.Command {
	return a.serviceActionCmd("start", "Start the installed service", "Started.", service.Manager.Start)
}

func (a *app) serviceStopCmd() *cobra.Command {
	return a.serviceActionCmd("stop", "Stop the service until the next login or `mycel service start`", "Stopped.", service.Manager.Stop)
}

func (a *app) serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the service is installed and running",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			m, logFile, err := a.serviceManager()
			if err != nil {
				return err
			}
			st, err := m.Status()
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(statusJSON(st, logFile))
			}
			yes := func(b bool) string {
				if b {
					return "yes"
				}
				return "no"
			}
			pid := "-"
			if st.PID > 0 {
				pid = fmt.Sprint(st.PID)
			}
			_, err = fmt.Fprintf(a.stdout, "installed: %s\nrunning:   %s\npid:       %s\nunit:      %s\nlog:       %s\n",
				yes(st.Installed), yes(st.Running), pid, st.Path, logFile)
			return err
		},
	}
}

func (a *app) serviceLogsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs [-f]",
		Short: "Print the daemon log (follow with -f)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logFile, err := service.LogFile(os.Getenv(config.EnvHome))
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return service.Tail(ctx, logFile, lines, follow, a.stdout)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "number of lines to print first")
	return cmd
}

// useLogFile sends the daemon's log to a rotating file (`mycel daemon
// --log-file`, which the service units pass).
func (a *app) useLogFile(path string) (io.Closer, error) {
	w, err := service.NewRotatingWriter(path, service.DefaultMaxLogBytes, service.DefaultLogFiles)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	level := slog.LevelInfo
	if a.verbose {
		level = slog.LevelDebug
	}
	a.log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(a.log) // packages that log through slog's default logger
	if err := redirectStderr(path); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("redirect stderr to the log file: %w", err)
	}
	return w, nil
}
