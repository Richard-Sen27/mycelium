package cli

// Phase 6: audit replay.
//
//	mycel audit ls [--trace id] [--actor a] [--resource kind:id] [--since t] [--limit n] [--json]

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func (a *app) auditCmd() *cobra.Command {
	cmd := groupCmd("audit", "Read the audit log")
	cmd.AddCommand(a.auditLsCmd())
	return cmd
}

// parseSince accepts an RFC 3339 time or a Go duration meaning "that long
// ago" ("1h").
func (a *app) parseSince(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return "", usagef("--since %q: want an RFC 3339 time or a duration such as 1h", v)
	}
	return a.now().Add(-d).UTC().Format(time.RFC3339), nil
}

func (a *app) auditLsCmd() *cobra.Command {
	var trace, actor, resource, since string
	var limit int32
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List audit entries, newest first (members see their own, their nodes' and their agents')",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 1000 {
				return usagef("--limit must be 1-1000")
			}
			from, err := a.parseSince(since)
			if err != nil {
				return err
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.events.ListAudit(cmd.Context(), connect.NewRequest(&mycelv1.ListAuditRequest{
				TraceId: trace, Actor: actor, Resource: resource, Since: from, Limit: limit,
			}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "TIME", "ACTOR", "ACTION", "RESOURCE", "OUTCOME", "TRACE")
			for _, e := range resp.Msg.GetEntries() {
				row(tw, e.GetAt(), e.GetActor(), e.GetAction(), dash(e.GetResource()), e.GetOutcome(), dash(e.GetTraceId()))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if resp.Msg.GetNextPageToken() != "" {
				_, err = fmt.Fprintln(a.stderr, "(more entries: narrow with --since, --actor or --resource, or raise --limit)")
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&trace, "trace", "", "only entries of this trace id")
	f.StringVar(&actor, "actor", "", "only entries by this actor (user id, node:<name>, agent:<name>@<node>, hub)")
	f.StringVar(&resource, "resource", "", "only entries about this resource (kind:id)")
	f.StringVar(&since, "since", "", "only entries at or after this time (RFC 3339, or a duration such as 1h)")
	f.Int32Var(&limit, "limit", 100, "at most this many entries (1-1000)")
	return cmd
}

// eventKindNames maps the CLI's kind names to event kinds.
var eventKindNames = map[string]mycelv1.EventKind{
	"node": mycelv1.EventKind_EVENT_KIND_NODE, "agent": mycelv1.EventKind_EVENT_KIND_AGENT,
	"channel": mycelv1.EventKind_EVENT_KIND_CHANNEL, "message": mycelv1.EventKind_EVENT_KIND_MESSAGE,
	"job": mycelv1.EventKind_EVENT_KIND_JOB, "grant": mycelv1.EventKind_EVENT_KIND_GRANT,
}

func eventKindName(k mycelv1.EventKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "EVENT_KIND_"))
}
