package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/hub"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Phase 7 (internal/hub/moving.go, internal/discovery): hub discover,
// export, import and redirect, and following a "hub moved" answer.

// hubMoveCmds are the phase 7 subcommands of `mycel hub`.
func (a *app) hubMoveCmds() []*cobra.Command {
	return []*cobra.Command{a.hubDiscoverCmd(), a.hubExportCmd(), a.hubImportCmd(), a.hubRedirectCmd()}
}

func (a *app) mdnsBrowser() discovery.Browser {
	if a.browser != nil {
		return a.browser
	}
	return discovery.Default()
}

// ---- discover ----

type discoveredHubJSON struct {
	TrustDomain   string   `json:"trust_domain"`
	URL           string   `json:"url"`
	BusURL        string   `json:"bus_url"`
	CAFingerprint string   `json:"ca_fingerprint"`
	Addresses     []string `json:"addresses"`
	Port          int      `json:"port"`
	Pinned        bool     `json:"pinned"` // the CA this CLI trusts
}

func (a *app) hubDiscoverCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "discover [--timeout 3s]",
		Short: "List hubs advertised on the local network (mDNS)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 || timeout > time.Minute {
				return usagef("--timeout must be between 0 and 1m")
			}
			pinned := ""
			if dirs, err := config.Resolve(); err == nil {
				if f, err := config.Load(dirs); err == nil && f.CACertPEM != "" {
					pinned, _ = identity.FingerprintPEM([]byte(f.CACertPEM))
				}
			}
			hubs, err := a.mdnsBrowser().Browse(cmd.Context(), timeout)
			if err != nil && len(hubs) == 0 {
				return fmt.Errorf("mDNS browse: %w", err)
			}
			out := make([]discoveredHubJSON, 0, len(hubs))
			for _, h := range hubs {
				addrs := h.Addresses
				if addrs == nil {
					addrs = []string{}
				}
				out = append(out, discoveredHubJSON{
					TrustDomain: h.TrustDomain, URL: h.URL, BusURL: h.BusURL, CAFingerprint: h.CAFingerprint,
					Addresses: addrs, Port: h.Port, Pinned: pinned != "" && pinned == h.CAFingerprint,
				})
			}
			if a.jsonOut {
				return a.printJSONValue(out)
			}
			if len(out) == 0 {
				_, err := fmt.Fprintf(a.stdout, "No hubs answered within %s.\n", timeout)
				return err
			}
			tw := newTable(a.stdout)
			row(tw, "TRUST DOMAIN", "URL", "BUS", "CA FINGERPRINT", "PINNED", "ADDRESSES")
			for _, h := range out {
				row(tw, cellText(h.TrustDomain), cellText(h.URL), cellText(dash(h.BusURL)), h.CAFingerprint[:16]+"…",
					yesNo(h.Pinned), cellText(strings.Join(h.Addresses, ",")))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.stdout, "\nAnswers are unauthenticated: trust a hub only by its CA fingerprint.")
			return err
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "how long to listen for answers")
	return cmd
}

// ---- passphrases ----

// readSecret reads one line from the terminal without echo.
func (a *app) readSecret(prompt string) ([]byte, error) {
	if a.readPassword != nil {
		return a.readPassword(prompt)
	}
	f, ok := a.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil, errors.New("no terminal to read the passphrase from; use --passphrase-file")
	}
	_, _ = fmt.Fprint(a.stderr, prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	_, _ = fmt.Fprintln(a.stderr)
	return b, err
}

// passphrase reads --passphrase-file or the terminal (twice when confirm).
func (a *app) passphrase(file string, confirm bool) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // the user names the file
		if err != nil {
			return nil, fmt.Errorf("passphrase file: %w", err)
		}
		b = bytes.TrimRight(b, "\r\n")
		if len(b) < hub.MinPassphrase {
			return nil, fmt.Errorf("the passphrase must have at least %d characters", hub.MinPassphrase)
		}
		return b, nil
	}
	p, err := a.readSecret("Export passphrase: ")
	if err != nil {
		return nil, err
	}
	if len(p) < hub.MinPassphrase {
		return nil, fmt.Errorf("the passphrase must have at least %d characters", hub.MinPassphrase)
	}
	if confirm {
		q, err := a.readSecret("Repeat the passphrase: ")
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(p, q) {
			return nil, errors.New("the passphrases differ")
		}
	}
	return p, nil
}

// ---- the local hub's API ----

// localHub is the running hub on this machine, reached over loopback with
// the hub directory's CA.
type localHub struct {
	url    string
	client *http.Client
}

// localHubURL is https://127.0.0.1:<port> for a wildcard or loopback API
// listen address, else the listen address itself.
func localHubURL(cfg *hub.Config) string {
	listen := cfg.APIListen
	if listen == "" {
		listen = ":7777"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || host == "localhost" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())) {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}

// runningHub returns the local hub's API when it answers /healthz, nil when
// the connection is refused (nothing listens). Any other failure is an
// error: the offline operations take hub.lock as well, so a hub that runs
// but does not answer is never exported or redirected behind its back.
func runningHub(ctx context.Context, hubDir string, cfg *hub.Config) (*localHub, error) {
	caPEM, err := os.ReadFile(filepath.Join(hubDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("hub ca.crt holds no certificate")
	}
	u := localHubURL(cfg)
	if u == "" {
		return nil, fmt.Errorf("hub.yaml api_listen %q is not host:port", cfg.APIListen)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, u+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, nil // nothing listens: stopped (hub.lock confirms it)
		}
		return nil, fmt.Errorf("cannot tell whether the hub at %s runs: %w", u, err)
	}
	_ = resp.Body.Close()
	return &localHub{url: u, client: c}, nil
}

// adminPost POSTs body to path on a hub with the admin API key.
func adminPost(ctx context.Context, c *http.Client, base, path, apiKey string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("%s: %s", path, e.Error)
	}
	return resp, nil
}

// ---- export ----

func (a *app) hubExportCmd() *cobra.Command {
	var out, passFile string
	var noArtifacts bool
	cmd := &cobra.Command{
		Use:   "export [--out file] [--passphrase-file f]",
		Short: "Write the hub's whole state to one passphrase-encrypted file (to move the hub)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dirs, f, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			hubDir := dirs.HubDir()
			cfg, err := hub.ReadConfig(hubDir)
			if err != nil {
				return fmt.Errorf("no hub on this machine (%s): %w", hubDir, err)
			}
			pass, err := a.passphrase(passFile, true)
			if err != nil {
				return err
			}
			if out == "" {
				out = fmt.Sprintf("hub-%s-%s.mycelhub", cfg.TrustDomain, a.now().UTC().Format("20060102"))
			}
			opts := hub.ExportOptions{HubDir: hubDir, Passphrase: pass, ExcludeArtifacts: noArtifacts, Logger: a.log}
			lh, err := runningHub(ctx, hubDir, cfg)
			if err != nil {
				return err
			}
			mode := "stopped hub"
			if lh != nil {
				if f.APIKey == "" {
					return fmt.Errorf("the hub is running; exporting it needs the admin API key in %s (or --api-key)", dirs.ConfigFile())
				}
				resp, err := adminPost(ctx, lh.client, lh.url, "/api/admin/export", f.APIKey, struct{}{})
				if err != nil {
					return fmt.Errorf("export from the running hub: %w", err)
				}
				defer func() { _ = resp.Body.Close() }()
				opts.Live = resp.Body
				mode = "running hub"
			}
			fh, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			err = hub.Export(ctx, opts, fh)
			if cerr := fh.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(out)
				return err
			}
			st, _ := os.Stat(out)
			size := int64(0)
			if st != nil {
				size = st.Size()
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"file": out, "bytes": size, "trust_domain": cfg.TrustDomain, "source": mode})
			}
			_, err = fmt.Fprintf(a.stdout, "Exported hub %s (%s) to %s (%s).\n"+
				"The file holds the CA key: keep it and the passphrase safe.\n\n"+
				"Next, on the new machine:\n  mycel hub import %s --advertise https://<new host>:7777\n",
				cfg.TrustDomain, mode, out, bytesString(uint64(max(size, 0))), filepath.Base(out))
			return err
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output file (default hub-<trust domain>-<date>.mycelhub)")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "read the passphrase from this file instead of the terminal")
	cmd.Flags().BoolVar(&noArtifacts, "exclude-artifacts", false, "leave stored artifacts out (old artifact downloads then fail on the new hub)")
	return cmd
}

// ---- import ----

func (a *app) hubImportCmd() *cobra.Command {
	opts := hub.ImportOptions{}
	var passFile, redirectFrom string
	cmd := &cobra.Command{
		Use:   "import <file> --advertise <url>",
		Short: "Restore an exported hub on this machine",
		Args:  exactArgs(1, "file"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if opts.AdvertiseURL == "" {
				return usagef("--advertise is required, e.g. --advertise https://new-hub.example.com:7777")
			}
			if _, err := normalizeHubURL(opts.AdvertiseURL); err != nil {
				return usageError{err}
			}
			if redirectFrom != "" {
				if _, err := normalizeHubURL(redirectFrom); err != nil {
					return usageError{err}
				}
			}
			dirs, f, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			src, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			pass, err := a.passphrase(passFile, false)
			if err != nil {
				return err
			}
			opts.HubDir, opts.Source, opts.Passphrase, opts.Logger = dirs.HubDir(), src, pass, a.log
			res, err := hub.Import(ctx, opts)
			if err != nil {
				return fmt.Errorf("hub import: %w", err)
			}
			var red *hub.RedirectResult
			if redirectFrom != "" {
				red, err = a.redirectRemote(ctx, f, redirectFrom, res)
				if err != nil {
					return fmt.Errorf("hub imported, but redirecting the old hub failed (run `mycel hub redirect %s` there): %w", res.AdvertiseURL, err)
				}
			}
			// Point this CLI at the imported hub when it used the old one.
			old := strings.TrimRight(f.HubURL, "/")
			if old == "" || old == strings.TrimRight(res.OldURL, "/") || old == strings.TrimRight(redirectFrom, "/") {
				if fresh, lerr := config.Load(dirs); lerr == nil {
					fresh.HubURL = res.AdvertiseURL
					if caPEM, rerr := os.ReadFile(filepath.Join(dirs.HubDir(), "ca.crt")); rerr == nil {
						fresh.CACertPEM = string(caPEM)
					}
					if serr := config.Save(dirs, fresh); serr != nil {
						a.log.Warn("could not update config.yaml", "err", serr)
					}
				}
			}
			if a.jsonOut {
				v := map[string]any{
					"trust_domain": res.TrustDomain, "hub_url": res.AdvertiseURL, "bus_url": res.BusAdvertise,
					"ca_fingerprint": res.CAFingerprint, "nodes": res.Nodes, "users": res.Users, "old_hub_url": res.OldURL,
					"relay_dropped": res.RelayDropped,
				}
				if red != nil {
					v["redirect"] = red
				}
				return a.printJSONValue(v)
			}
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Hub %s imported (%d nodes, %d users).\n\n", res.TrustDomain, res.Nodes, res.Users)
			tw := newTable(w)
			row(tw, "  Hub URL:", res.AdvertiseURL)
			row(tw, "  Bus:", res.BusAdvertise)
			row(tw, "  CA fingerprint:", res.CAFingerprint)
			_ = tw.Flush()
			if res.RelayDropped {
				_, _ = fmt.Fprintln(w, "\nThe old hub used a relay; its relay.url was not imported. Set up a relay token for this hub (a relay token belongs to one hub).")
			}
			_, _ = fmt.Fprintf(w, "\nNext steps:\n  mycel daemon                       start the hub here\n")
			if red != nil {
				_, err = fmt.Fprintf(w, "The old hub %s now redirects %d nodes here (signed with the hub CA).\n", redirectFrom, red.Nodes)
				return err
			}
			_, err = fmt.Fprintf(w, "  mycel hub redirect %s\n                                     on the old hub, so every node follows\n", res.AdvertiseURL)
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&opts.AdvertiseURL, "advertise", "", "URL nodes use to reach this hub, e.g. https://hub.example.com:7777 (required)")
	fl.StringVar(&opts.BusAdvertise, "bus-advertise", "", "bus address given to nodes (default tls://<advertise host>:<bus port>)")
	fl.StringVar(&opts.APIListen, "api-listen", "", "API and UI listen address (default: as exported)")
	fl.StringVar(&opts.BusListen, "bus-listen", "", "bus listen address (default: as exported)")
	fl.StringVar(&redirectFrom, "redirect-from", "", "old hub URL: redirect it here with the admin key from config.yaml")
	fl.StringVar(&passFile, "passphrase-file", "", "read the passphrase from this file instead of the terminal")
	return cmd
}

// redirectRemote asks the old hub to redirect to the imported one. The
// old hub must present the imported CA.
func (a *app) redirectRemote(ctx context.Context, f *config.File, oldURL string, res *hub.ImportResult) (*hub.RedirectResult, error) {
	if f.APIKey == "" {
		return nil, errors.New("no admin API key in config.yaml (use `mycel login` or --api-key)")
	}
	caPEM := []byte(f.CACertPEM)
	if fp, err := identity.FingerprintPEM(caPEM); err != nil || fp != res.CAFingerprint {
		return nil, errors.New("config.yaml does not pin the imported hub's CA; log in to the old hub first")
	}
	hc, err := pinnedHTTPClient(caPEM)
	if err != nil {
		return nil, err
	}
	resp, err := adminPost(ctx, hc, oldURL, "/api/admin/redirect", f.APIKey, map[string]string{"hub_url": res.AdvertiseURL, "bus_url": res.BusAdvertise})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out hub.RedirectResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- redirect ----

func (a *app) hubRedirectCmd() *cobra.Command {
	var busURL string
	var force bool
	cmd := &cobra.Command{
		Use:   "redirect <new hub url> [--bus host:port]",
		Short: "On the old hub: sign a redirect to the new hub, send it to every node and stop serving",
		Args:  exactArgs(1, "new hub url"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			newURL := strings.TrimRight(args[0], "/")
			if _, err := normalizeHubURL(newURL); err != nil {
				return usageError{err}
			}
			dirs, f, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			hubDir := dirs.HubDir()
			cfg, err := hub.ReadConfig(hubDir)
			if err != nil {
				return fmt.Errorf("no hub on this machine (%s): %w", hubDir, err)
			}
			tHub, tBus, err := hub.NormalizeRedirect(cfg, newURL, busURL)
			if err != nil {
				return usageError{err}
			}
			if !force {
				caPEM, err := os.ReadFile(filepath.Join(hubDir, "ca.crt")) //nolint:gosec // the hub's own CA
				if err != nil {
					return err
				}
				if err := checkRedirectTarget(ctx, caPEM, tHub, tBus); err != nil {
					return fmt.Errorf("the new hub does not answer as this hub's CA at %s (bus %s): %w; start it first, or pass --force", tHub, tBus, err)
				}
			}
			lh, err := runningHub(ctx, hubDir, cfg)
			if err != nil {
				return err
			}
			var res *hub.RedirectResult
			if lh != nil {
				if f.APIKey == "" {
					return fmt.Errorf("the hub is running; the redirect needs the admin API key in %s (or --api-key)", dirs.ConfigFile())
				}
				resp, err := adminPost(ctx, lh.client, lh.url, "/api/admin/redirect", f.APIKey, map[string]string{"hub_url": newURL, "bus_url": busURL})
				if err != nil {
					return err
				}
				defer func() { _ = resp.Body.Close() }()
				res = &hub.RedirectResult{}
				if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(res); err != nil {
					return err
				}
			} else {
				res, err = hub.Redirect(ctx, hub.RedirectOptions{HubDir: hubDir, HubURL: newURL, BusURL: busURL, Logger: a.log})
				if err != nil {
					return fmt.Errorf("hub redirect: %w", err)
				}
			}
			if a.jsonOut {
				return a.printJSONValue(res)
			}
			when := "queued to"
			if lh == nil {
				when = "sent on the next start to"
			}
			_, err = fmt.Fprintf(a.stdout, "This hub now redirects to %s (bus %s).\nThe signed redirect is %s %d nodes; every call to this hub is answered with it.\n"+
				"CLIs follow with --follow-redirect. To undo: remove moved_to from %s and restart the hub.\n",
				res.HubURL, res.BusURL, when, res.Nodes, filepath.Join(hubDir, "hub.yaml"))
			return err
		},
	}
	cmd.Flags().StringVar(&busURL, "bus", "", "bus address of the new hub (default: its host with this hub's bus port)")
	cmd.Flags().BoolVar(&force, "force", false, "redirect even when the new hub does not answer yet (nodes keep their address until it does)")
	return cmd
}

// checkRedirectTarget checks the new hub before the old one points nodes
// at it: /healthz over TLS verified against the hub CA only (never the
// system roots), and a TLS handshake with its bus verified the same way.
func checkRedirectTarget(ctx context.Context, caPEM []byte, hubURL, busURL string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("hub ca.crt holds no certificate")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("API: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API: /healthz answered %s", resp.Status)
	}
	if err := checkBusTLS(ctx, pool, busURL); err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	return nil
}

// checkBusTLS dials the bus and completes the TLS handshake against pool.
// The bus sends its INFO line in clear text first and then upgrades; the
// handshake verifies the bus certificate (no client certificate is sent,
// so the bus then closes the connection).
func checkBusTLS(ctx context.Context, pool *x509.CertPool, busURL string) error {
	hostPort := strings.TrimPrefix(busURL, "tls://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", hostPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, 64<<10), 64<<10).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "INFO ") {
		return errors.New("no bus answered there")
	}
	tc := tls.Client(conn, &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS13})
	return tc.HandshakeContext(dctx)
}

// ---- following "hub moved" ----

// hubMovedError is a call answered with a signed redirect.
type hubMovedError struct{ r *mycelv1.Redirect }

func (e hubMovedError) Error() string { return "hub moved to " + e.r.GetHubUrl() }

// followMoved verifies the redirect against the CA in config.yaml, prints
// the new address and, with --follow-redirect or after a y/N prompt, updates
// config.yaml, remembering issued_at so an older redirect is refused later.
// The command itself still failed (exit 1).
func (a *app) followMoved(m hubMovedError) int {
	_, _ = fmt.Fprintf(a.stderr, "mycel: %v\n", m)
	dirs, err := config.Resolve()
	if err != nil {
		return exitError
	}
	f, err := config.Load(dirs)
	if err != nil || f.CACertPEM == "" {
		_, _ = fmt.Fprintln(a.stderr, "mycel: cannot verify the redirect: no pinned hub CA in config.yaml")
		return exitError
	}
	if err := daemon.VerifyRedirect([]byte(f.CACertPEM), m.r, a.now()); err != nil {
		_, _ = fmt.Fprintf(a.stderr, "mycel: the redirect is not valid for the pinned hub CA; not followed: %v\n", err)
		return exitError
	}
	if m.r.GetIssuedAt() < f.RedirectIssuedAt {
		_, _ = fmt.Fprintln(a.stderr, "mycel: the redirect is older than one already followed; not followed")
		return exitError
	}
	newURL := strings.TrimRight(m.r.GetHubUrl(), "/")
	_, _ = fmt.Fprintf(a.stderr, "The redirect is signed by the pinned hub CA. New hub address: %s\n", newURL)
	follow := a.followRedirect
	if !follow && !a.jsonOut && a.isTTY() {
		_, _ = fmt.Fprintf(a.stderr, "Update %s to use %s? [y/N] ", dirs.ConfigFile(), newURL)
		line, _ := bufio.NewReader(a.stdin).ReadString('\n')
		follow = strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
	}
	if !follow {
		_, _ = fmt.Fprintln(a.stderr, "Run the command again with --follow-redirect to switch config.yaml to the new hub.")
		return exitError
	}
	f.HubURL = newURL
	f.RedirectIssuedAt = m.r.GetIssuedAt()
	if err := config.Save(dirs, f); err != nil {
		_, _ = fmt.Fprintf(a.stderr, "mycel: saving %s: %v\n", dirs.ConfigFile(), err)
		return exitError
	}
	_, _ = fmt.Fprintf(a.stderr, "%s now points at %s; run the command again.\n", dirs.ConfigFile(), newURL)
	return exitError
}
