package cli

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func (a *app) tokenCmd() *cobra.Command {
	cmd := groupCmd("token", "Create, list and revoke node join tokens")
	cmd.AddCommand(a.tokenCreateCmd(), a.tokenLsCmd(), a.tokenRevokeCmd())
	return cmd
}

func (a *app) tokenCreateCmd() *cobra.Command {
	var (
		name string
		caps []string
		ttl  string
	)
	cmd := &cobra.Command{
		Use:   "create --name <node> [--caps docker,ollama,gpu] [--ttl 15m]",
		Short: "Create a one-time join token for a new node",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return usagef("--name is required")
			}
			if _, err := time.ParseDuration(ttl); err != nil {
				return usagef("invalid --ttl %q: %v", ttl, err)
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.CreateJoinTokenRequest{
				NodeName: name,
				Caps:     &mycelv1.Capabilities{Names: splitList(caps)},
				Ttl:      ttl,
			}
			a.log.Debug("creating join token", "hub", hc.url, "node", name, "ttl", ttl)
			resp, err := hc.tokens.CreateJoinToken(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			jt := resp.Msg.GetJoinToken()
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Join token for node %q (caps %s, expires %s).\n", jt.GetNodeName(), capsString(jt.GetCaps()), dash(jt.GetExpiresAt()))
			_, _ = fmt.Fprintf(w, "Run this on the new machine; the token works once:\n\n")
			_, err = fmt.Fprintf(w, "  mycel join %s\n", resp.Msg.GetToken())
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the node that will join (required)")
	f.StringSliceVar(&caps, "caps", nil, "capability ceiling, comma-separated (docker,ollama,gpu,sandbox)")
	f.StringVar(&ttl, "ttl", "15m", "how long the token stays valid")
	return cmd
}

func (a *app) tokenLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List join tokens",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.tokens.ListJoinTokens(cmd.Context(), connect.NewRequest(&mycelv1.ListJoinTokensRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "ID", "NODE", "CAPS", "EXPIRES", "USED")
			for _, t := range resp.Msg.GetTokens() {
				used := dash(t.GetUsedAt())
				if t.GetRevokedAt() != "" {
					used = "revoked " + t.GetRevokedAt()
				}
				row(tw, t.GetId(), t.GetNodeName(), capsString(t.GetCaps()), dash(t.GetExpiresAt()), used)
			}
			return tw.Flush()
		},
	}
}

func (a *app) tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an unused join token",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.tokens.RevokeJoinToken(cmd.Context(), connect.NewRequest(&mycelv1.RevokeJoinTokenRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Revoked join token %s.\n", args[0])
			return err
		},
	}
}
