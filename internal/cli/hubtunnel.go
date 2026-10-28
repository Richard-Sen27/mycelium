package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

// hubTunnelCmd: `mycel hub tunnel set|unset|status` (phase 7).
func (a *app) hubTunnelCmd() *cobra.Command {
	cmd := groupCmd("tunnel", "Expose this hub through a relay")
	cmd.AddCommand(a.hubTunnelSetCmd(), a.hubTunnelUnsetCmd(), a.hubTunnelStatusCmd())
	return cmd
}

// parsePortRoles parses "443=api,7778=bus".
func parsePortRoles(s string) (map[int]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[int]string{}
	for _, f := range strings.Split(s, ",") {
		p, role, ok := strings.Cut(strings.TrimSpace(f), "=")
		n, err := strconv.Atoi(p)
		if !ok || err != nil {
			return nil, fmt.Errorf("invalid port mapping %q (want port=api or port=bus)", f)
		}
		out[n] = role
	}
	if err := (tunnel.Config{Ports: out}).Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// readRelayURL returns the relay URL argument. "-" reads it from stdin (one
// line), so the secret stays out of argv and shell history; a URL given on
// the command line works but draws a warning.
func (a *app) readRelayURL(arg string) (string, error) {
	if arg != "-" {
		_, _ = fmt.Fprintln(a.stderr, "Warning: the relay URL contains a secret; pass `-` and paste it (or pipe it in) to keep it out of your shell history.")
		return arg, nil
	}
	if a.isTTY() {
		_, _ = fmt.Fprint(a.stderr, "Relay URL: ")
	}
	line, err := bufio.NewReader(io.LimitReader(a.stdin, 4<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", usagef("no relay URL on stdin")
	}
	return line, nil
}

func (a *app) hubTunnelSetCmd() *cobra.Command {
	var ports string
	cmd := &cobra.Command{
		Use:   "set - [--ports 443=api,7778=bus]",
		Short: "Store the relay URL (read from stdin with `-`) and expose the hub through it (applies on the next hub start)",
		Args:  exactArgs(1, "- (relay URL on stdin)"),
		RunE: func(_ *cobra.Command, args []string) error {
			pm, err := parsePortRoles(ports)
			if err != nil {
				return usagef("--ports: %v", err)
			}
			raw, err := a.readRelayURL(args[0])
			if err != nil {
				return err
			}
			if _, err := tunnel.ParseRelayURL(raw); err != nil {
				return usageError{err}
			}
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			res, err := hub.SetTunnel(dirs.HubDir(), raw, pm)
			if err != nil {
				return fmt.Errorf("hub tunnel set: %w", err)
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{
					"relay": res.Relay, "advertise_url": res.AdvertiseURL, "bus_advertise": res.BusAdvertise,
					"cert_reissued": res.CertReissued,
				})
			}
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Tunnel set to %s.\n", res.Relay)
			_, _ = fmt.Fprintf(w, "  Advertise URL: %s\n  Bus advertise: %s\n", res.AdvertiseURL, res.BusAdvertise)
			if res.CertReissued {
				_, _ = fmt.Fprintf(w, "  The server certificate was re-issued to cover the relay host.\n")
			}
			_, err = fmt.Fprintf(w, "Restart the daemon to connect (`mycel hub tunnel status` shows the state). Nodes that joined before keep their hub address.\n")
			return err
		},
	}
	cmd.Flags().StringVar(&ports, "ports", "", "public relay ports and their role (default 443=api,7778=bus)")
	return cmd
}

func (a *app) hubTunnelUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset",
		Short: "Stop using the relay (applies on the next hub start)",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			if err := hub.UnsetTunnel(dirs.HubDir()); err != nil {
				return fmt.Errorf("hub tunnel unset: %w", err)
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"enabled": false})
			}
			_, err = fmt.Fprintln(a.stdout, "Tunnel removed; restart the daemon to apply. advertise_url in hub.yaml is unchanged.")
			return err
		},
	}
}

// tunnelStatus mirrors GET /api/hub/tunnel.
type tunnelStatus struct {
	Enabled   bool              `json:"enabled"`
	Ports     map[string]string `json:"ports,omitempty"`
	Error     string            `json:"error,omitempty"`
	Relay     string            `json:"relay,omitempty"`
	Connected bool              `json:"connected"`
	Since     *time.Time        `json:"since,omitempty"`
	Connects  uint64            `json:"connects"`
	Active    int64             `json:"active"`
	Total     uint64            `json:"total"`
	LastError string            `json:"last_error,omitempty"`
	Replaced  uint64            `json:"replaced,omitempty"`
	ACME      struct {
		Enabled bool   `json:"enabled"`
		Host    string `json:"host,omitempty"`
		Ready   bool   `json:"ready"`
	} `json:"acme"`
}

func (a *app) hubTunnelStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the hub's tunnel and ACME status (asks the running hub)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			st, raw, err := hc.tunnelStatus(ctx)
			if err != nil {
				return err
			}
			if a.jsonOut {
				_, err := a.stdout.Write(raw)
				return err
			}
			return a.printTunnelStatus(st)
		},
	}
}

func (hc *hubClient) tunnelStatus(ctx context.Context) (*tunnelStatus, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hc.url+"/api/hub/tunnel", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", hc.authz.header)
	res, err := hc.raw.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("hub unreachable at %s: %w", hc.url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, nil, fmt.Errorf("hub at %s: %w", hc.url, err)
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		switch res.StatusCode {
		case http.StatusUnauthorized:
			return nil, nil, fmt.Errorf("authentication failed: %s (check your API key, or %s)", e.Error, loginHint)
		case http.StatusForbidden:
			return nil, nil, fmt.Errorf("permission denied: %s", e.Error)
		case http.StatusNotFound:
			return nil, nil, errors.New("this hub does not report tunnel status (older version?)")
		}
		return nil, nil, fmt.Errorf("hub error (%s) at %s: %s", res.Status, hc.url, e.Error)
	}
	var st tunnelStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, nil, fmt.Errorf("hub at %s: invalid tunnel status: %w", hc.url, err)
	}
	return &st, body, nil
}

func (a *app) printTunnelStatus(st *tunnelStatus) error {
	tw := newTable(a.stdout)
	if !st.Enabled {
		row(tw, "Tunnel:", "disabled")
	} else {
		state := "disconnected"
		if st.Connected {
			state = "connected"
		}
		if st.Since != nil && !st.Since.IsZero() {
			state += " since " + st.Since.Local().Format(time.RFC3339)
		}
		row(tw, "Tunnel:", "enabled")
		if st.Relay != "" {
			row(tw, "Relay:", st.Relay)
		}
		ports := make([]string, 0, len(st.Ports))
		for p, r := range st.Ports {
			ports = append(ports, p+"="+r)
		}
		sort.Strings(ports)
		row(tw, "Ports:", strings.Join(ports, ","))
		if st.Error != "" {
			row(tw, "State:", "not running: "+st.Error)
		} else {
			row(tw, "State:", state)
			row(tw, "Connections:", fmt.Sprintf("%d open, %d total, %d sessions", st.Active, st.Total, st.Connects))
		}
		if st.LastError != "" {
			row(tw, "Last error:", st.LastError)
		}
		if st.Replaced > 0 {
			row(tw, "Replaced:", fmt.Sprintf("%d times by another session of this hub (another hub using the same relay URL?)", st.Replaced))
		}
	}
	if st.ACME.Enabled {
		state := "waiting for the certificate; browsers get the hub CA certificate"
		if st.ACME.Ready {
			state = "certificate ready"
		}
		row(tw, "ACME:", "enabled for "+st.ACME.Host+" ("+state+")")
	} else {
		row(tw, "ACME:", "disabled")
	}
	return tw.Flush()
}

// ---- hub init --tunnel / --acme ----

// hubInitTunnelOpts holds the phase 7 flags of `mycel hub init`.
type hubInitTunnelOpts struct {
	relayURL  string
	acme      bool
	acmeEmail string
}

func (o *hubInitTunnelOpts) addFlags(f *pflag.FlagSet) {
	f.StringVar(&o.relayURL, "tunnel", "", "`-` to read the relay URL from `mycel relay token create` on stdin (exposes the hub through the relay; the advertise URL is the relay's)")
	f.BoolVar(&o.acme, "acme", false, "get a Let's Encrypt certificate for browsers on the advertise host (TLS-ALPN-01: the advertise URL must use port 443; "+
		"if --api-listen is another port, forward public port 443 to it)")
	f.StringVar(&o.acmeEmail, "acme-email", "", "contact email for the ACME account")
}

// sameHubURL compares two https URLs by scheme, host (case-insensitive) and
// port (443 when absent), ignoring a trailing slash.
func sameHubURL(a, b string) bool {
	ua, err1 := url.Parse(strings.TrimRight(a, "/"))
	ub, err2 := url.Parse(strings.TrimRight(b, "/"))
	if err1 != nil || err2 != nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		return "443"
	}
	return ua.Scheme == ub.Scheme && strings.EqualFold(ua.Hostname(), ub.Hostname()) && port(ua) == port(ub) &&
		ua.Path == "" && ub.Path == "" && ua.RawQuery == "" && ub.RawQuery == ""
}

// prepare validates the flags before anything is created and fills in the
// advertise addresses for a tunnelled hub. A relay URL of "-" is read from
// stdin.
func (o *hubInitTunnelOpts) prepare(a *app, opts *hub.InitOptions) error {
	if o.acmeEmail != "" && !o.acme {
		return usagef("--acme-email needs --acme")
	}
	var tc *tunnel.Config
	if o.relayURL != "" {
		raw, err := a.readRelayURL(o.relayURL)
		if err != nil {
			return err
		}
		o.relayURL = raw
		ru, err := tunnel.ParseRelayURL(o.relayURL)
		if err != nil {
			return usageError{err}
		}
		tc = &tunnel.Config{Enabled: true, Ports: tunnel.DefaultPorts()}
		adv, bus, err := hub.TunnelAdvertise(ru.Host, tc.Ports)
		if err != nil {
			return err
		}
		if opts.AdvertiseURL == "" {
			opts.AdvertiseURL = adv
		} else if !sameHubURL(opts.AdvertiseURL, adv) {
			return usagef("--advertise %q differs from the tunnel's advertise URL %q; leave --advertise out with --tunnel", opts.AdvertiseURL, adv)
		}
		if opts.BusAdvertise == "" {
			opts.BusAdvertise = bus
		}
		// This machine's node and CLI use the local listeners, not the relay.
		opts.LocalListeners = true
	}
	if o.acme {
		if opts.AdvertiseURL == "" {
			return usagef("--acme needs --advertise https://<public dns name>")
		}
		if err := tunnel.ValidateACME(opts.AdvertiseURL, o.acmeEmail, tc); err != nil {
			return usageError{err}
		}
	}
	return nil
}

// apply writes the tunnel and ACME settings into the new hub.
func (o *hubInitTunnelOpts) apply(hubDir string) error {
	if o.relayURL != "" {
		if _, err := hub.SetTunnel(hubDir, o.relayURL, nil); err != nil {
			return err
		}
	}
	if o.acme {
		if err := hub.SetACME(hubDir, tunnel.ACMEConfig{Enabled: true, Email: o.acmeEmail}); err != nil {
			return err
		}
	}
	return nil
}
