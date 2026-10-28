package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
)

// hubUIURL is the admin UI served by the hub role on this machine.
const hubUIURL = "https://localhost:7777"

func (a *app) uiCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Print and open the local UI URL",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := "http://" + localAddr
			if !cmd.Flags().Changed("local-addr") {
				dirs, err := config.Resolve()
				if err != nil {
					return err
				}
				if fileExists(filepath.Join(dirs.HubDir(), "hub.yaml")) {
					url = hubUIURL
				}
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"url": url})
			}
			a.openURL(url)
			_, err := fmt.Fprintln(a.stdout, url)
			return err
		},
	}
	cmd.Flags().StringVar(&localAddr, "local-addr", defaultLocalAddr, "address of the local node UI")
	return cmd
}

// openBrowser opens url with the platform opener, ignoring failures.
func openBrowser(url string) {
	name, args := "xdg-open", []string{url}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	cmd := exec.Command(name, args...) //nolint:gosec // fixed opener; url is built from a host:port, no shell involved
	if err := cmd.Start(); err != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
