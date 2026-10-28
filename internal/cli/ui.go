package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/hub"
)

// uiCmd opens the node UI signed in. It asks the daemon for a single-use
// sign-in code (POST /api/local/ui-code with the local token) and opens
// http://<local addr>/#code=<code>; the app trades the code for the token,
// so the token never appears on a command line or in a URL. --print prints
// the #local=<token> URL instead (for tests and scripts).
func (a *app) uiCmd() *cobra.Command {
	var localAddr string
	var printURL bool
	cmd := &cobra.Command{
		Use:   "ui [--print]",
		Short: "Open this machine's node UI in the browser, signed in",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, tok, err := a.localTarget(cmd, localAddr)
			if err != nil {
				return err
			}
			if printURL {
				full := base + "/#local=" + tok
				if a.jsonOut {
					return a.printJSONValue(map[string]string{"url": full})
				}
				_, err := fmt.Fprintln(a.stdout, full)
				return err
			}
			code, err := uiSignInCode(cmd.Context(), base, tok)
			if err != nil {
				return err
			}
			a.openURL(base + "/#code=" + code)
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"url": base + "/", "opened": true})
			}
			_, err = fmt.Fprintf(a.stdout, "Opened %s/ in your browser (the sign-in link works once, for 60 s).\n", base)
			return err
		},
	}
	localAddrFlag(cmd, &localAddr)
	cmd.Flags().BoolVar(&printURL, "print", false, "print a sign-in URL that contains the local token instead of opening a browser")
	return cmd
}

// uiSignInCode asks the daemon for a single-use UI sign-in code.
func uiSignInCode(ctx context.Context, base, tok string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/local/ui-code", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // base is a checked loopback URL (localTarget)
	if err != nil {
		return "", fmt.Errorf("the node daemon at %s did not answer (is `mycel daemon` running?): %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Code == "" {
		msg := out.Error
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("the node daemon refused a sign-in code: %s", msg)
	}
	return out.Code, nil
}

// hubUIURLCmd prints the hub UI address: the hub's external URL from
// hub.yaml on the hub machine, else the configured hub URL.
func (a *app) hubUIURLCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui-url",
		Short: "Print the hub UI address",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u, err := a.hubUIURL(cmd)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"url": u})
			}
			_, err = fmt.Fprintln(a.stdout, u)
			return err
		},
	}
}

func (a *app) hubUIURL(cmd *cobra.Command) (string, error) {
	dirs, f, err := a.loadConfig(cmd)
	if err != nil {
		return "", err
	}
	if !cmd.Flags().Changed("hub-url") && fileExists(filepath.Join(dirs.HubDir(), "hub.yaml")) {
		hc, err := hub.ReadConfig(dirs.HubDir())
		if err != nil {
			return "", err
		}
		for _, u := range []string{hc.UI.ExternalURL, hc.AdvertiseURL} {
			if u != "" {
				return strings.TrimRight(u, "/"), nil
			}
		}
	}
	if f.HubURL != "" {
		return strings.TrimRight(f.HubURL, "/"), nil
	}
	return "", errors.New("no hub configured here: " + loginHint)
}

// openBrowser opens url with the platform opener, ignoring failures. The
// URL is passed as one argument, never through a shell.
func openBrowser(url string) {
	name, args := "xdg-open", []string{url}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	cmd := exec.Command(name, args...) //nolint:gosec // fixed opener; the URL is one argument, no shell involved
	if err := cmd.Start(); err != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
