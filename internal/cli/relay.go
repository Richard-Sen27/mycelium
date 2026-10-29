package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

// relayCmd: `mycel relay serve|token` (phase 7, internal/tunnel).
func (a *app) relayCmd() *cobra.Command {
	cmd := groupCmd("relay", "Run a relay that exposes a hub behind NAT")
	cmd.AddCommand(a.relayServeCmd(), a.relayTokenCmd())
	return cmd
}

// relayDataDir resolves --data-dir (default <data dir>/relay).
func relayDataDir(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	dirs, err := config.Resolve()
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.Data, "relay"), nil
}

// parsePorts parses "443,7778" into public port -> listen address
// (":443"). An entry may name the address to bind, "127.0.0.1:443" or
// "[::1]:443", e.g. for a relay with several addresses; the port is what
// hubs map in their tunnel ports.
func parsePorts(s string) (map[int]string, error) {
	out := map[int]string{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		host, ps := "", f
		if strings.Contains(f, ":") {
			h, p, err := net.SplitHostPort(f)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", f)
			}
			host, ps = h, p
		}
		p, err := strconv.Atoi(ps)
		if err != nil || p < 1 || p > 65535 || strings.Trim(ps, "0123456789") != "" { // no "+443"
			return nil, fmt.Errorf("invalid port %q", f)
		}
		addr := net.JoinHostPort(host, strconv.Itoa(p))
		if prev, ok := out[p]; ok && prev != addr {
			return nil, fmt.Errorf("port %d given twice", p)
		}
		out[p] = addr
	}
	if len(out) == 0 {
		return nil, errors.New("no ports given")
	}
	return out, nil
}

func (a *app) relayServeCmd() *cobra.Command {
	var listen, public, dataDir string
	cmd := &cobra.Command{
		Use:   "serve [--listen :7790] [--public 443,7778] [--data-dir d]",
		Short: "Run the relay (control listener for the hub, public ports forwarded to it)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ports, err := parsePorts(public)
			if err != nil {
				return usagef("--public: %v", err)
			}
			if _, _, err := net.SplitHostPort(listen); err != nil {
				return usagef("--listen %q: %v", listen, err)
			}
			dir, err := relayDataDir(dataDir)
			if err != nil {
				return err
			}
			r, err := tunnel.NewRelay(tunnel.RelayOptions{DataDir: dir, Listen: listen, Public: ports, Logger: a.log})
			if err != nil {
				return err
			}
			if !a.jsonOut {
				_, _ = fmt.Fprintf(a.stdout, "Relay running: control %s, public ports %s, certificate fingerprint %s\n", r.ControlAddr(), public, r.Fingerprint())
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return r.Serve(ctx)
		},
	}
	f := cmd.Flags()
	f.StringVar(&listen, "listen", ":"+strconv.Itoa(tunnel.DefaultControlPort), "control listener the hub connects to")
	f.StringVar(&public, "public", "443,7778", "public ports forwarded to the hub (comma-separated; an entry may be host:port to bind one address)")
	f.StringVar(&dataDir, "data-dir", "", "relay state: certificate and token hashes (default <data dir>/relay)")
	return cmd
}

func (a *app) relayTokenCmd() *cobra.Command {
	cmd := groupCmd("token", "Create, list and revoke relay tokens for hubs")
	cmd.AddCommand(a.relayTokenCreateCmd(), a.relayTokenLsCmd(), a.relayTokenRevokeCmd())
	return cmd
}

func (a *app) relayTokenCreateCmd() *cobra.Command {
	var name, host, dataDir string
	var replace bool
	var port int
	cmd := &cobra.Command{
		Use:   "create --name <hub> [--host relay.example.com] [--port 7790] [--data-dir d]",
		Short: "Create a token for a hub and print its relay URL (shown once)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return usagef("--name is required")
			}
			if !tunnel.ValidHubName(name) {
				return usagef("invalid --name %q (lowercase letters, digits, '-')", name)
			}
			if port < 1 || port > 65535 {
				return usagef("invalid --port %d", port)
			}
			if host == "" {
				h, err := os.Hostname()
				if err != nil || h == "" {
					return usagef("--host is required (the relay's public host name)")
				}
				host = strings.ToLower(h)
				_, _ = fmt.Fprintf(a.stderr, "Warning: --host not given; the URL names this machine's hostname %q. Hubs, nodes and browsers must reach the relay under this name; pass --host <public name> otherwise.\n", host)
			}
			dir, err := relayDataDir(dataDir)
			if err != nil {
				return err
			}
			_, fp, err := tunnel.LoadOrCreateRelayCert(dir)
			if err != nil {
				return err
			}
			// Check the URL before any token changes (--replace revokes).
			probe := &tunnel.RelayURL{Host: host, Port: port, Name: name, Fingerprint: fp, Token: strings.Repeat("A", 43)}
			if _, err := tunnel.ParseRelayURL(probe.String()); err != nil {
				return usagef("invalid --host %q: %v", host, err)
			}
			secret, err := tunnel.CreateToken(dir, name, replace)
			if errors.Is(err, tunnel.ErrTokenExists) {
				return fmt.Errorf("%w; pass --replace to revoke it and create a new one", err)
			}
			if err != nil {
				return err
			}
			u := &tunnel.RelayURL{Host: host, Port: port, Name: name, Fingerprint: fp, Token: secret}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"name": name, "url": u.String(), "fingerprint": fp})
			}
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Relay token for hub %q. The URL contains the secret and is shown once:\n\n", name)
			_, _ = fmt.Fprintf(w, "  %s\n\n", u.String())
			_, err = fmt.Fprintf(w, "On the hub machine run `mycel hub tunnel set -` (or `mycel hub init --tunnel - ...`)\nand paste the URL, or pipe it in, so it stays out of shell history.\n")
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "hub name (required)")
	f.BoolVar(&replace, "replace", false, "revoke the relay's existing token (one hub per relay) and create a new one")
	f.StringVar(&host, "host", "", "public host name of this relay (default: hostname, with a warning)")
	f.IntVar(&port, "port", tunnel.DefaultControlPort, "control port of this relay")
	f.StringVar(&dataDir, "data-dir", "", "relay state directory (default <data dir>/relay)")
	return cmd
}

func (a *app) relayTokenLsCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "ls [--data-dir d]",
		Short: "List relay tokens (names only; secrets are not stored)",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			dir, err := relayDataDir(dataDir)
			if err != nil {
				return err
			}
			toks, err := tunnel.ListTokens(dir)
			if err != nil {
				return err
			}
			if a.jsonOut {
				type tok struct {
					Name      string `json:"name"`
					CreatedAt string `json:"created_at"`
				}
				out := make([]tok, 0, len(toks))
				for _, t := range toks {
					out = append(out, tok{Name: t.Name, CreatedAt: t.CreatedAt})
				}
				return a.printJSONValue(map[string]any{"tokens": out})
			}
			tw := newTable(a.stdout)
			row(tw, "NAME", "CREATED")
			for _, t := range toks {
				row(tw, t.Name, t.CreatedAt)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "relay state directory (default <data dir>/relay)")
	return cmd
}

func (a *app) relayTokenRevokeCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "revoke <name> [--data-dir d]",
		Short: "Revoke a hub's relay token (a running relay drops its session)",
		Args:  exactArgs(1, "name"),
		RunE: func(_ *cobra.Command, args []string) error {
			dir, err := relayDataDir(dataDir)
			if err != nil {
				return err
			}
			if err := tunnel.RevokeToken(dir, args[0]); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"revoked": args[0]})
			}
			_, err = fmt.Fprintf(a.stdout, "Relay token for %q revoked.\n", args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "relay state directory (default <data dir>/relay)")
	return cmd
}
