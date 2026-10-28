// Package cli implements the mycel command-line interface (cobra).
//
// Contract (owned by the orchestrator): the command tree below is the phase-1
// surface. Flags in brackets are optional. Every command supports --json for
// machine-readable output; the e2e tests depend on it.
//
//	mycel version
//	mycel hub init  --advertise <url> [--trust-domain <td>] [--api-listen :7777] [--bus-listen :7778] [--admin <name>] [--node-name <name>]
//	                 prints the admin API key once and writes config.yaml (hub_url, api_key, ca pem)
//	mycel join <token> [--hub-url <url>]
//	mycel daemon    [--api-listen] [--bus-listen] [--local-addr 127.0.0.1:7777]
//	                 runs every role present in the data dir (docs/decisions.md D006)
//	mycel token create --name <node> [--caps docker,ollama,gpu] [--ttl 15m]
//	mycel token ls
//	mycel token revoke <id>
//	mycel nodes ls
//	mycel nodes get <name>
//	mycel nodes rm <name>
//	mycel nodes caps <name> --set docker,ollama
//	mycel user add <name> --role admin|member      (prints the new user's API key once)
//	mycel user ls
//	mycel user rm <id>
//	mycel whoami
//	mycel login <hub-url> --api-key <key> [--ca-fingerprint <hex>]
//	                 fetches <hub-url>/ca.pem, checks the fingerprint (asks if not given, unless --json), writes config.yaml
//	mycel ui         prints (and tries to open) the local UI URL
//
// Phase 2 (agents and channels):
//
//	mycel mcp --agent <name> [--harness <name>] [--local-addr]   MCP server on stdio (internal/adapters/mcp)
//	mycel hook --agent <name> [--local-addr]                     new inbox messages for harness hooks; always exits 0
//	mycel agent setup <harness> --agent <name> [--dir .]         writes the harness config from harnesses/<harness>.yaml
//	mycel agents ls [--local]
//	mycel channels ls [--all] | create <name> [--type topic|task] [--max-hops N] [--rate N] [--open] [--members a,b]
//	mycel channels get <name> | archive <name> | add <channel> <member> [--role r] | rm <channel> <member>
//	mycel post <channel> <text> [--mentions a,b] [--reply-to id] [--thread id] [--type chat|task|result|event] [--tags t]
//	mycel read <channel> [--limit 50] [--thread id] [--follow] [--quote]
//	mycel bb get <channel> [key] | put <channel> <key> <value> [--expect-version n] | rm <channel> <key>
//
// Phase 3 (orchestrator sessions, local workers, budgets):
//
//	mycel agents ls                     (adds PARENT, STATE with pause reason, USED tokens/$usd)
//	mycel agents get|pause|resume|stop <id>     (pause [--reason text])
//	mycel agents budget <id> [--tokens N] [--usd X]
//	mycel channels create ... [--budget-tokens N] [--budget-usd X]
//	mycel channels policy <name> [--budget-tokens N] [--budget-usd X] [--max-hops N] [--rate N] [--open]
//	mycel channels usage <name> | reset-usage <name>
//	mycel agent spawn <harness> --task <text> --channel <name> [--name] [--workdir] [--max-turns] [--budget-tokens] [--budget-usd]
//	mycel workers ls | stop <agent>                              (local API)
//	mycel fakeharness --script <file> --agent <name> [--prompt]  (hidden; build tag fakeharness)
//
// Commands that take --local-addr default to $MYCEL_LOCAL_ADDR when set.
//
// A channel argument without "#", "@" or "dm:" gets a leading "#".
//
// Global flags: --json, --hub-url, --api-key (override config.yaml), -v.
// Environment: MYCEL_HOME (see internal/config), MYCEL_API_KEY, MYCEL_HUB_URL.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/version"
)

// Environment variables that override config.yaml (flags override these).
const (
	EnvHubURL = "MYCEL_HUB_URL"
	EnvAPIKey = "MYCEL_API_KEY" //nolint:gosec // variable name, not a credential
)

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitRevoked = 3
)

// extraCommands are added to the root by files compiled in only with a build
// tag (the hidden `fakeharness` command, tag fakeharness).
var extraCommands []func(*app) *cobra.Command

// usageError marks a command-line mistake (exit code 2).
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// exitCodeError carries a specific exit code; err may be nil (nothing to print).
type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.err.Error()
}
func (e exitCodeError) Unwrap() error { return e.err }

// app holds the per-invocation state shared by all commands.
type app struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	// Injectable for tests.
	isTTY   func() bool
	openURL func(url string)
	now     func() time.Time
	baseCtx context.Context // nil: context.Background()

	// Role entry points (internal/hub, internal/daemon); tests use fakes.
	hubInit    func(context.Context, hub.InitOptions) (*hub.InitResult, error)
	hubRun     func(context.Context, hub.Options, chan<- hub.Ready) error
	daemonJoin func(context.Context, daemon.JoinOptions) (*daemon.JoinResult, error)
	daemonRun  func(context.Context, daemon.Options, chan<- daemon.Ready) error

	jsonOut bool
	hubURL  string
	apiKey  string
	verbose bool

	log *slog.Logger
}

// Main runs the CLI with the given arguments and returns the process exit code.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr, os.Stdin)
}

func run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	return newApp(stdout, stderr, stdin).execute(args)
}

func newApp(stdout, stderr io.Writer, stdin io.Reader) *app {
	return &app{
		stdout:     stdout,
		stderr:     stderr,
		stdin:      stdin,
		isTTY:      func() bool { return isTerminal(stdin) },
		openURL:    openBrowser,
		now:        time.Now,
		hubInit:    hub.Init,
		hubRun:     hub.Run,
		daemonJoin: daemon.Join,
		daemonRun:  daemon.Run,
	}
}

// groupCmd is a command that only holds subcommands: without arguments it
// prints help, with an unknown subcommand it is a usage error.
func groupCmd(use, short string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:     use,
		Short:   short,
		Aliases: aliases,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usagef("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		},
	}
}

func (a *app) execute(args []string) int {
	root := a.rootCmd()
	root.SetArgs(args)
	ctx := a.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var ec exitCodeError
	if errors.As(err, &ec) {
		if ec.err != nil {
			_, _ = fmt.Fprintf(a.stderr, "mycel: %v\n", ec.err)
		}
		return ec.code
	}
	_, _ = fmt.Fprintf(a.stderr, "mycel: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) || strings.HasPrefix(err.Error(), "unknown command") {
		_, _ = fmt.Fprintln(a.stderr, "Run 'mycel --help' for usage.")
		return exitUsage
	}
	return exitError
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mycel",
		Short:         "Mycelium: one network for AI agents and the compute they may use",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if a.verbose {
				level = slog.LevelDebug
			}
			a.log = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))
			return nil
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetIn(a.stdin)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })

	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", false, "machine-readable JSON output")
	pf.StringVar(&a.hubURL, "hub-url", "", "hub URL (overrides config.yaml and "+EnvHubURL+")")
	pf.StringVar(&a.apiKey, "api-key", "", "API key (overrides config.yaml and "+EnvAPIKey+")")
	pf.BoolVarP(&a.verbose, "verbose", "v", false, "debug logging to stderr")

	root.AddCommand(
		a.versionCmd(),
		a.hubCmd(),
		a.joinCmd(),
		a.daemonCmd(),
		a.tokenCmd(),
		a.nodesCmd(),
		a.userCmd(),
		a.whoamiCmd(),
		a.loginCmd(),
		a.uiCmd(),
		a.mcpCmd(),
		a.hookCmd(),
		a.agentCmd(),
		a.agentsCmd(),
		a.channelsCmd(),
		a.postCmd(),
		a.readCmd(),
		a.bbCmd(),
		a.workersCmd(),
	)
	for _, mk := range extraCommands {
		root.AddCommand(mk(a))
	}
	return root
}

// exactArgs is cobra.ExactArgs with a usage error and a readable message.
func exactArgs(n int, names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			want := strings.Join(names, " ")
			return usagef("%s: expected %d argument(s) <%s>, got %d", cmd.CommandPath(), n, want, len(args))
		}
		return nil
	}
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usagef("%s: unexpected argument %q", cmd.CommandPath(), args[0])
	}
	return nil
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the mycel version",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"version": version.Version})
			}
			_, err := fmt.Fprintln(a.stdout, version.Version)
			return err
		},
	}
}

// overrideFlag returns the value of a global override, flags winning over env.
func (a *app) overrideFlag(cmd *cobra.Command, flag, env, val string) (string, bool) {
	if cmd.Flags().Changed(flag) {
		return val, true
	}
	if v := os.Getenv(env); v != "" {
		return v, true
	}
	return "", false
}

// loadConfig loads config.yaml and applies the env and flag overrides.
func (a *app) loadConfig(cmd *cobra.Command) (config.Dirs, *config.File, error) {
	dirs, err := config.Resolve()
	if err != nil {
		return config.Dirs{}, nil, fmt.Errorf("resolve config directory: %w", err)
	}
	f, err := config.Load(dirs)
	if err != nil {
		return config.Dirs{}, nil, err
	}
	if v, ok := a.overrideFlag(cmd, "hub-url", EnvHubURL, a.hubURL); ok {
		f.HubURL = v
	}
	if v, ok := a.overrideFlag(cmd, "api-key", EnvAPIKey, a.apiKey); ok {
		f.APIKey = v
	}
	return dirs, f, nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
