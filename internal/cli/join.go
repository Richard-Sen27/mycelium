package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
)

type joinJSON struct {
	NodeName     string   `json:"node_name"`
	SPIFFEID     string   `json:"spiffe_id"`
	HubURL       string   `json:"hub_url"`
	BusURL       string   `json:"bus_url"`
	OwnerID      string   `json:"owner_id"`
	Caps         []string `json:"caps"`
	CertNotAfter string   `json:"cert_not_after"`
}

func (a *app) joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <token> [--hub-url <url>]",
		Short: "Enrol this machine as a node with a one-time join token",
		Args:  exactArgs(1, "token"),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := strings.TrimSpace(args[0])
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			opts := daemon.JoinOptions{NodeDir: dirs.NodeDir(), Token: token, Logger: a.log}
			if cmd.Flags().Changed("hub-url") {
				opts.HubURL = a.hubURL
			}
			res, err := a.daemonJoin(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("join: %w", err)
			}
			if err := a.saveJoinConfig(dirs, res); err != nil {
				return err
			}

			if a.jsonOut {
				caps := res.Caps
				if caps == nil {
					caps = []string{}
				}
				return a.printJSONValue(joinJSON{
					NodeName:     res.NodeName,
					SPIFFEID:     res.SPIFFEID,
					HubURL:       res.HubURL,
					BusURL:       res.BusURL,
					OwnerID:      res.OwnerID,
					Caps:         caps,
					CertNotAfter: res.CertNotAfter,
				})
			}
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Joined %s as node %s.\n\n", res.HubURL, res.NodeName)
			tw := newTable(w)
			row(tw, "  SPIFFE ID:", res.SPIFFEID)
			row(tw, "  Owner:", dash(res.OwnerID))
			row(tw, "  Caps ceiling:", dash(strings.Join(res.Caps, ",")))
			row(tw, "  Cert expires:", dash(res.CertNotAfter))
			_ = tw.Flush()
			_, err = fmt.Fprintf(w, "\nNext: run `mycel daemon` to connect and report inventory.\n")
			return err
		},
	}
}

// saveJoinConfig records the join in config.yaml. The pinned CA is only ever
// written together with the hub URL it belongs to: a CLI already logged into
// a different hub is left untouched.
func (a *app) saveJoinConfig(dirs config.Dirs, res *daemon.JoinResult) error {
	f, err := config.Load(dirs)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(filepath.Join(dirs.NodeDir(), "ca.crt"))
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		a.log.Warn("joined, but the node state has no ca.crt; CLI commands need `mycel login`", "dir", dirs.NodeDir())
	default:
		return err
	}
	joined := strings.TrimRight(res.HubURL, "/")
	switch strings.TrimRight(f.HubURL, "/") {
	case "", joined:
		f.HubURL = joined
		if len(caPEM) > 0 {
			f.CACertPEM = string(caPEM)
		}
		f.NodeName = res.NodeName
	default:
		a.log.Warn("the CLI is logged into a different hub; config.yaml left unchanged",
			"config_hub", f.HubURL, "joined_hub", joined, "config", dirs.ConfigFile())
		return nil
	}
	if err := config.Save(dirs, f); err != nil {
		return fmt.Errorf("joined, but saving %s failed: %w", dirs.ConfigFile(), err)
	}
	return nil
}
