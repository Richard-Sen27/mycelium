package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	mcpadapter "github.com/richard-sen27/mycelium/internal/adapters/mcp"
)

var normChannel = mcpadapter.NormalizeChannel

func (a *app) channelsCmd() *cobra.Command {
	cmd := groupCmd("channels", "List and manage channels", "channel", "ch")
	cmd.AddCommand(a.channelsLsCmd(), a.channelsCreateCmd(), a.channelsGetCmd(),
		a.channelsArchiveCmd(), a.channelsAddCmd(), a.channelsRmCmd())
	return cmd
}

func (a *app) channelsLsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List channels",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.chans.ListChannels(cmd.Context(), connect.NewRequest(&mycelv1.ListChannelsRequest{IncludeArchived: all}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "NAME", "TYPE", "OWNER", "MEMBERS", "LAST SEQ", "CREATED", "ARCHIVED")
			for _, c := range resp.Msg.GetChannels() {
				row(tw, c.GetName(), mcpadapter.ChannelTypeName(c.GetType()), dash(c.GetOwnerId()),
					strconv.Itoa(len(c.GetMembers())), strconv.FormatUint(c.GetLastSeq(), 10),
					ago(c.GetCreatedAt(), a.now()), dash(c.GetArchivedAt()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include archived channels")
	return cmd
}

func parseChannelType(s string) (mycelv1.ChannelType, error) {
	switch strings.ToLower(s) {
	case "topic":
		return mycelv1.ChannelType_CHANNEL_TYPE_TOPIC, nil
	case "task":
		return mycelv1.ChannelType_CHANNEL_TYPE_TASK, nil
	default:
		return mycelv1.ChannelType_CHANNEL_TYPE_UNSPECIFIED, usagef("invalid channel type %q: want topic or task", s)
	}
}

func (a *app) channelsCreateCmd() *cobra.Command {
	var typ string
	var maxHops, rate int32
	var open bool
	var members []string
	cmd := &cobra.Command{
		Use:   "create <name> [--type topic|task] [--max-hops N] [--rate N] [--open] [--members a,b]",
		Short: "Create a channel",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ct, err := parseChannelType(typ)
			if err != nil {
				return err
			}
			if maxHops < 0 || rate < 0 {
				return usagef("--max-hops and --rate must not be negative")
			}
			req := &mycelv1.CreateChannelRequest{Name: normChannel(args[0]), Type: ct, Members: splitList(members)}
			if cmd.Flags().Changed("max-hops") || cmd.Flags().Changed("rate") || open {
				// Zero values mean "hub default"; both kinds of members may
				// post on a topic or task channel.
				req.Policy = &mycelv1.ChannelPolicy{
					MaxHops: maxHops, RatePerMinute: rate, OpenToMembers: open,
					AgentsMayPost: true, HumansMayPost: true,
				}
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.chans.CreateChannel(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			c := resp.Msg.GetChannel()
			_, err = fmt.Fprintf(a.stdout, "Created %s channel %s.\n", mcpadapter.ChannelTypeName(c.GetType()), cellText(c.GetName()))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&typ, "type", "topic", "channel type: topic or task")
	f.Int32Var(&maxHops, "max-hops", 0, "longest reply chain allowed (0 = hub default, 8)")
	f.Int32Var(&rate, "rate", 0, "messages per minute per agent (0 = hub default, 60)")
	f.BoolVar(&open, "open", false, "any member may join without an invite")
	f.StringSliceVar(&members, "members", nil, "initial members (agent or user ids), comma-separated")
	return cmd
}

func (a *app) channelsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a channel with its policy and members",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.chans.GetChannel(cmd.Context(), connect.NewRequest(&mycelv1.GetChannelRequest{Name: normChannel(args[0])}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			c := resp.Msg.GetChannel()
			p := c.GetPolicy()
			tw := newTable(a.stdout)
			kv := func(k, v string) { row(tw, k+":", v) }
			kv("Name", c.GetName())
			kv("Type", mcpadapter.ChannelTypeName(c.GetType()))
			kv("Owner", dash(c.GetOwnerId()))
			kv("Created", dash(c.GetCreatedAt()))
			kv("Archived", dash(c.GetArchivedAt()))
			kv("Last seq", strconv.FormatUint(c.GetLastSeq(), 10))
			kv("Max hops", strconv.Itoa(int(p.GetMaxHops())))
			kv("Rate/min", strconv.Itoa(int(p.GetRatePerMinute())))
			kv("Agents may post", yesNo(p.GetAgentsMayPost()))
			kv("Humans may post", yesNo(p.GetHumansMayPost()))
			kv("Open to members", yesNo(p.GetOpenToMembers()))
			if err := tw.Flush(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(a.stdout)
			tw = newTable(a.stdout)
			row(tw, "MEMBER", "ROLE", "JOINED")
			for _, m := range c.GetMembers() {
				row(tw, m.GetId(), dash(m.GetRole()), dash(m.GetJoinedAt()))
			}
			return tw.Flush()
		},
	}
}

func (a *app) channelsArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <name>",
		Short: "Archive a channel",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			name := normChannel(args[0])
			resp, err := hc.chans.ArchiveChannel(cmd.Context(), connect.NewRequest(&mycelv1.ArchiveChannelRequest{Name: name}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Archived %s.\n", name)
			return err
		},
	}
}

func (a *app) channelsAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add <channel> <member> [--role agent|human|observer|owner]",
		Short: "Add a member (agent or user id) to a channel",
		Args:  exactArgs(2, "channel", "member"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.AddMemberRequest{Channel: normChannel(args[0]), Member: args[1], Role: role}
			resp, err := hc.chans.AddMember(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Added %s to %s.\n", args[1], req.GetChannel())
			return err
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "member role (default: agent for agents, human for users)")
	return cmd
}

func (a *app) channelsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <channel> <member>",
		Short: "Remove a member from a channel",
		Args:  exactArgs(2, "channel", "member"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.RemoveMemberRequest{Channel: normChannel(args[0]), Member: args[1]}
			resp, err := hc.chans.RemoveMember(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Removed %s from %s.\n", args[1], req.GetChannel())
			return err
		},
	}
}

func (a *app) postCmd() *cobra.Command {
	var mentions, tags []string
	var replyTo, thread, typ string
	cmd := &cobra.Command{
		Use:   "post <channel> <text>",
		Short: "Post a message to a channel (or @agent:<name>@<node> for a direct message)",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usagef("%s: expected arguments <channel> <text>", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			mt, err := mcpadapter.ParseMessageType(typ)
			if err != nil {
				return usageError{err}
			}
			req := &mycelv1.PostRequest{
				Channel: normChannel(args[0]), Text: strings.Join(args[1:], " "),
				Thread: thread, Mentions: splitList(mentions), Type: mt, ReplyTo: replyTo, Tags: splitList(tags),
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.chans.Post(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintln(a.stdout, mcpadapter.PostedLine(resp.Msg.GetMessage()))
			return err
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&mentions, "mentions", nil, "agent or user ids to mention, comma-separated")
	f.StringVar(&replyTo, "reply-to", "", "id of the message this one answers")
	f.StringVar(&thread, "thread", "", "thread id to post into")
	f.StringVar(&typ, "type", "chat", "message type: chat, task, result or event")
	f.StringSliceVar(&tags, "tags", nil, "tags, comma-separated")
	return cmd
}

func (a *app) readCmd() *cobra.Command {
	var limit int32
	var thread string
	var follow, quote bool
	cmd := &cobra.Command{
		Use:   "read <channel> [--limit n] [--thread id] [--follow]",
		Short: "Read a channel's messages, oldest first; --follow streams new ones",
		Args:  exactArgs(1, "channel"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return usagef("--limit must not be negative")
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ch := normChannel(args[0])
			resp, err := hc.chans.ListMessages(cmd.Context(), connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: ch, Thread: thread, Limit: limit}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			msgs := resp.Msg.GetMessages()
			sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].GetSeq() < msgs[j].GetSeq() })
			if a.jsonOut {
				if err := a.printProto(resp.Msg); err != nil {
					return err
				}
			} else {
				for _, m := range msgs {
					a.printMessage(m, quote)
				}
			}
			if !follow {
				return nil
			}
			var after uint64
			if n := len(msgs); n > 0 {
				after = msgs[n-1].GetSeq()
			}
			return a.follow(cmd.Context(), hc, ch, thread, after, quote)
		},
	}
	f := cmd.Flags()
	f.Int32Var(&limit, "limit", 50, "number of recent messages (max 500)")
	f.StringVar(&thread, "thread", "", "only this thread")
	f.BoolVarP(&follow, "follow", "f", false, "keep streaming new messages")
	f.BoolVar(&quote, "quote", false, "print messages as the quoted blocks agents see")
	return cmd
}

func (a *app) follow(ctx context.Context, hc *hubClient, ch, thread string, after uint64, quote bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	stream, err := hc.stream.Watch(ctx, connect.NewRequest(&mycelv1.WatchRequest{Channels: []string{ch}, AfterSeq: after}))
	if err != nil {
		return rpcError(hc.url, err)
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		m := stream.Msg().GetMessage()
		if m == nil || (after > 0 && m.GetSeq() <= after) {
			continue
		}
		if thread != "" && m.GetThread() != thread {
			continue
		}
		if a.jsonOut {
			if err := a.printProto(stream.Msg()); err != nil {
				return err
			}
			continue
		}
		a.printMessage(m, quote)
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		return rpcError(hc.url, err)
	}
	return nil
}

// printMessage prints "[ts] channel from: text", or the agent's quoted block.
func (a *app) printMessage(m *mycelv1.Message, quote bool) {
	if quote {
		_, _ = fmt.Fprintf(a.stdout, "%s\n\n", termText(mcpadapter.RenderMessages([]*mycelv1.Message{m})))
		return
	}
	_, _ = fmt.Fprintf(a.stdout, "[%s] %s %s: %s\n", cellText(m.GetTs()), cellText(m.GetChannel()), cellText(m.GetFrom()), termText(m.GetPayload().GetText()))
}

func (a *app) bbCmd() *cobra.Command {
	cmd := groupCmd("bb", "Read and write a channel's blackboard", "blackboard")
	cmd.AddCommand(a.bbGetCmd(), a.bbPutCmd(), a.bbRmCmd())
	return cmd
}

func (a *app) bbGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <channel> [key]",
		Short: "List a blackboard, or print one key's value",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usagef("%s: expected arguments <channel> [key]", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.BlackboardGetRequest{Channel: normChannel(args[0])}
			if len(args) == 2 {
				req.Key = args[1]
			}
			resp, err := hc.chans.BlackboardGet(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			entries := resp.Msg.GetEntries()
			if req.GetKey() != "" {
				for _, e := range entries {
					if e.GetKey() == req.GetKey() {
						_, err := fmt.Fprintln(a.stdout, termText(e.GetValue()))
						return err
					}
				}
				return fmt.Errorf("not found: key %q on the blackboard of %s", req.GetKey(), req.GetChannel())
			}
			tw := newTable(a.stdout)
			row(tw, "KEY", "VERSION", "UPDATED BY", "UPDATED", "VALUE")
			for _, e := range entries {
				row(tw, e.GetKey(), strconv.FormatUint(e.GetVersion(), 10), dash(e.GetUpdatedBy()),
					ago(e.GetUpdatedAt(), a.now()), shorten(e.GetValue(), 60))
			}
			return tw.Flush()
		},
	}
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (a *app) bbPutCmd() *cobra.Command {
	var expect uint64
	cmd := &cobra.Command{
		Use:   "put <channel> <key> <value>",
		Short: "Write a blackboard key",
		Args:  exactArgs(3, "channel", "key", "value"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.BlackboardPutRequest{Channel: normChannel(args[0]), Key: args[1], Value: args[2], ExpectVersion: expect}
			resp, err := hc.chans.BlackboardPut(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			e := resp.Msg.GetEntry()
			_, err = fmt.Fprintf(a.stdout, "Stored %s on the blackboard of %s (version %d).\n", cellText(e.GetKey()), cellText(e.GetChannel()), e.GetVersion())
			return err
		},
	}
	cmd.Flags().Uint64Var(&expect, "expect-version", 0, "only write if the entry is at this version (0 = unconditional)")
	return cmd
}

func (a *app) bbRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <channel> <key>",
		Short: "Delete a blackboard key",
		Args:  exactArgs(2, "channel", "key"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.BlackboardDeleteRequest{Channel: normChannel(args[0]), Key: args[1]}
			resp, err := hc.chans.BlackboardDelete(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Removed %s from the blackboard of %s.\n", args[1], req.GetChannel())
			return err
		},
	}
}
