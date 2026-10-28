package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/hub"
)

func (a *app) hubCmd() *cobra.Command {
	cmd := groupCmd("hub", "Manage the hub role on this machine")
	cmd.AddCommand(a.hubInitCmd(), a.hubUIURLCmd())
	return cmd
}

type hubInitJSON struct {
	TrustDomain   string `json:"trust_domain"`
	HubURL        string `json:"hub_url"`
	CAFingerprint string `json:"ca_fingerprint"`
	AdminUserID   string `json:"admin_user_id"`
	AdminAPIKey   string `json:"admin_api_key"`
	LocalNodeName string `json:"local_node_name"`
}

func (a *app) hubInitCmd() *cobra.Command {
	opts := hub.InitOptions{}
	cmd := &cobra.Command{
		Use:   "init --advertise <url>",
		Short: "Switch on the hub role here and enrol this machine as its first node",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.AdvertiseURL == "" {
				return usagef("--advertise is required, e.g. --advertise https://hub.example.com:7777")
			}
			if _, err := normalizeHubURL(opts.AdvertiseURL); err != nil {
				return usageError{err}
			}
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			opts.HubDir = dirs.HubDir()
			opts.NodeDir = dirs.NodeDir()
			opts.Logger = a.log
			a.log.Debug("initialising hub", "hub_dir", opts.HubDir, "advertise", opts.AdvertiseURL)
			res, err := a.hubInit(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("hub init: %w", err)
			}
			hubURL := res.AdvertiseURL
			if hubURL == "" {
				hubURL = opts.AdvertiseURL
			}

			// The admin key exists only in res: print it before anything else
			// can fail.
			if err := a.printHubInit(res, hubURL, adminName(opts.AdminName), dirs.ConfigFile()); err != nil {
				return err
			}

			f, err := config.Load(dirs)
			if err == nil {
				f.HubURL = hubURL
				f.APIKey = res.AdminAPIKey
				f.CACertPEM = string(res.CACertPEM)
				f.NodeName = res.LocalNodeName
				err = config.Save(dirs, f)
			}
			if err != nil {
				return fmt.Errorf("hub initialised, but saving %s failed (keep the admin key printed above): %w", dirs.ConfigFile(), err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.AdvertiseURL, "advertise", "", "URL nodes use to reach the hub API, e.g. https://hub.example.com:7777 (required)")
	f.StringVar(&opts.TrustDomain, "trust-domain", "", "SPIFFE trust domain (default: host of --advertise)")
	f.StringVar(&opts.APIListen, "api-listen", ":7777", "API and UI listen address")
	f.StringVar(&opts.BusListen, "bus-listen", ":7778", "bus listen address")
	f.StringVar(&opts.AdminName, "admin", "admin", "name of the first admin user")
	f.StringVar(&opts.LocalNode, "node-name", "", "node name for this machine (default: hostname)")
	return cmd
}

func (a *app) printHubInit(res *hub.InitResult, hubURL, admin, configFile string) error {
	if a.jsonOut {
		return a.printJSONValue(hubInitJSON{
			TrustDomain:   res.TrustDomain,
			HubURL:        hubURL,
			CAFingerprint: res.CAFingerprint,
			AdminUserID:   res.AdminUserID,
			AdminAPIKey:   res.AdminAPIKey,
			LocalNodeName: res.LocalNodeName,
		})
	}
	w := a.stdout
	_, _ = fmt.Fprintf(w, "Hub initialised.\n\n")
	tw := newTable(w)
	row(tw, "  Trust domain:", res.TrustDomain)
	row(tw, "  Hub URL:", hubURL)
	row(tw, "  CA fingerprint:", res.CAFingerprint)
	row(tw, "  Admin user:", fmt.Sprintf("%s (%s)", admin, res.AdminUserID))
	row(tw, "  Local node:", res.LocalNodeName)
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nAdmin API key (shown once; this CLI stores it in %s):\n\n  %s\n\n", configFile, res.AdminAPIKey)
	_, _ = fmt.Fprintf(w, "Next steps:\n")
	_, _ = fmt.Fprintf(w, "  mycel daemon                       start the hub and this machine's node\n")
	_, _ = fmt.Fprintf(w, "  mycel token create --name <node>   create a join token for another machine\n")
	_, err := fmt.Fprintf(w, "  mycel login %s --api-key <key> --ca-fingerprint %s\n                                     use the hub from another machine\n", hubURL, res.CAFingerprint)
	return err
}

func adminName(n string) string {
	if n == "" {
		return "admin"
	}
	return n
}
