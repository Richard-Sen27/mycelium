package cli

// Phase 6: search.
//
//	mycel search <query> [--kind node,agent,channel,message,job,grant] [--json]

import (
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func (a *app) searchCmd() *cobra.Command {
	var kinds []string
	var limit int32
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search nodes, agents, channels, messages, jobs and grants you may see",
		Args:  exactArgs(1, "query"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" || len(args[0]) > 200 {
				return usagef("the query must be 1-200 bytes")
			}
			if limit < 1 || limit > 200 {
				return usagef("--limit must be 1-200")
			}
			req := &mycelv1.SearchRequest{Query: args[0], Limit: limit}
			for _, k := range kinds {
				for _, name := range strings.Split(k, ",") {
					name = strings.ToLower(strings.TrimSpace(name))
					if name == "" {
						continue
					}
					v, ok := eventKindNames[name]
					if !ok {
						return usagef("--kind %q: want node, agent, channel, message, job or grant", name)
					}
					if !slices.Contains(req.Kinds, v) {
						req.Kinds = append(req.Kinds, v)
					}
				}
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.events.Search(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "KIND", "ID", "TITLE", "MATCH", "AT")
			for _, h := range resp.Msg.GetHits() {
				row(tw, eventKindName(h.GetKind()), h.GetId(), shorten(h.GetTitle(), 40), shorten(dash(h.GetSnippet()), 60), dash(h.GetAt()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds: node, agent, channel, message, job, grant")
	cmd.Flags().Int32Var(&limit, "limit", 50, "at most this many hits (1-200)")
	return cmd
}
