package cli

// Phase 5: kill switch and sharing settings.
//
//	mycel freeze --agent id | --channel name | --all [--reason r]
//	mycel unfreeze --agent id | --channel name | --all
//	mycel freeze status
//	mycel nodes share <name> [sharing flags]          (hub, ControlService)
//	mycel node share [sharing flags]                  (this machine, local API; tighten only)
//	mycel node grants ls | revoke <id>                (this machine, local API)
//	mycel node freeze status                          (this machine, local API)

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ---- kill switch ----

// freezeTarget is the one scope a freeze or unfreeze acts on.
type freezeTarget struct {
	agent, channel string
	all            bool
}

func freezeFlags(cmd *cobra.Command, t *freezeTarget) {
	f := cmd.Flags()
	f.StringVar(&t.agent, "agent", "", "pause one agent (kill switch level one)")
	f.StringVar(&t.channel, "channel", "", "freeze a channel: its agents pause and posts are refused")
	f.BoolVar(&t.all, "all", false, "freeze the whole network: every sandbox and worker pauses, every broker denies")
}

// scope checks that exactly one of --agent, --channel and --all was given and
// returns "agent", "channel" or "all".
func (t *freezeTarget) scope(cmd *cobra.Command) (string, error) {
	var set []string
	if cmd.Flags().Changed("agent") {
		set = append(set, "agent")
	}
	if cmd.Flags().Changed("channel") {
		set = append(set, "channel")
	}
	if cmd.Flags().Changed("all") {
		set = append(set, "all")
	}
	if len(set) != 1 {
		return "", usagef("%s: give exactly one of --agent, --channel or --all", cmd.CommandPath())
	}
	switch {
	case set[0] == "agent" && strings.TrimSpace(t.agent) == "":
		return "", usagef("--agent needs an agent id")
	case set[0] == "channel" && strings.TrimSpace(t.channel) == "":
		return "", usagef("--channel needs a channel name")
	case set[0] == "all" && !t.all:
		return "", usagef("--all=false selects nothing; give exactly one of --agent, --channel or --all")
	}
	return set[0], nil
}

func (a *app) freezeCmd() *cobra.Command {
	var t freezeTarget
	var reason string
	cmd := &cobra.Command{
		Use:   "freeze --agent id | --channel name | --all [--reason r]",
		Short: "Kill switch: pause an agent, freeze a channel or freeze the whole network",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, err := t.scope(cmd)
			if err != nil {
				return err
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var target, msg string
			switch scope {
			case "agent":
				target = normAgentID(t.agent)
				_, err = hc.agents.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: target, Reason: reason}))
				msg = "Paused " + target + "."
			case "channel":
				target = normChannel(t.channel)
				_, err = hc.control.FreezeChannel(ctx, connect.NewRequest(&mycelv1.FreezeChannelRequest{Channel: target, Reason: reason}))
				msg = "Froze " + target + ": its agents are paused and posts are refused."
			default:
				target = "network"
				_, err = hc.control.FreezeAll(ctx, connect.NewRequest(&mycelv1.FreezeAllRequest{Reason: reason}))
				msg = "Froze the network: every sandbox and worker is paused and every broker denies. Undo with `mycel unfreeze --all`."
			}
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"scope": scope, "target": target, "frozen": true})
			}
			_, err = fmt.Fprintln(a.stdout, cellText(msg))
			return err
		},
	}
	freezeFlags(cmd, &t)
	cmd.Flags().StringVar(&reason, "reason", "", "why (shown with the frozen state and audited)")
	cmd.AddCommand(a.freezeStatusCmd())
	return cmd
}

func (a *app) unfreezeCmd() *cobra.Command {
	var t freezeTarget
	cmd := &cobra.Command{
		Use:   "unfreeze --agent id | --channel name | --all",
		Short: "Undo a freeze (the network needs an admin)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, err := t.scope(cmd)
			if err != nil {
				return err
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var target, msg string
			switch scope {
			case "agent":
				target = normAgentID(t.agent)
				_, err = hc.agents.ResumeAgent(ctx, connect.NewRequest(&mycelv1.ResumeAgentRequest{Id: target}))
				msg = "Resumed " + target + "."
			case "channel":
				target = normChannel(t.channel)
				_, err = hc.control.UnfreezeChannel(ctx, connect.NewRequest(&mycelv1.UnfreezeChannelRequest{Channel: target}))
				msg = "Unfroze " + target + "."
			default:
				target = "network"
				_, err = hc.control.UnfreezeAll(ctx, connect.NewRequest(&mycelv1.UnfreezeAllRequest{}))
				msg = "Unfroze the network."
			}
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"scope": scope, "target": target, "frozen": false})
			}
			_, err = fmt.Fprintln(a.stdout, cellText(msg))
			return err
		},
	}
	freezeFlags(cmd, &t)
	return cmd
}

func (a *app) freezeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the network and which channels are frozen",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.control.FreezeStatus(cmd.Context(), connect.NewRequest(&mycelv1.FreezeStatusRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			s := resp.Msg
			tw := newTable(a.stdout)
			if s.GetAllFrozen() {
				v := "frozen"
				if s.GetAllSince() != "" {
					v += " since " + s.GetAllSince()
				}
				if s.GetAllBy() != "" {
					v += " by " + s.GetAllBy()
				}
				row(tw, "Network:", v)
				row(tw, "Reason:", dash(s.GetAllReason()))
			} else {
				row(tw, "Network:", "not frozen")
			}
			chans := "none"
			if len(s.GetFrozenChannels()) > 0 {
				chans = strings.Join(s.GetFrozenChannels(), ", ")
			}
			row(tw, "Frozen channels:", chans)
			return tw.Flush()
		},
	}
}

// ---- sharing settings ----

var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// parseWindow parses a --window value: days, "=", then from-to in local
// time, e.g. "mon-fri=22:00-07:00" or "sat,sun=00:00-24:00". Day ranges may
// wrap ("fri-mon"); "to" may be "24:00"; a window that ends before it starts
// runs past midnight.
func parseWindow(s string) (*mycelv1.ScheduleWindow, error) {
	daysPart, timesPart, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "=")
	if !ok || daysPart == "" || timesPart == "" {
		return nil, fmt.Errorf("invalid window %q: want days=HH:MM-HH:MM, e.g. mon-fri=22:00-07:00", s)
	}
	seen := map[int]bool{}
	for _, item := range strings.Split(daysPart, ",") {
		item = strings.TrimSpace(item)
		from, to, isRange := strings.Cut(item, "-")
		i := slices.Index(weekdays, from)
		j := i
		if isRange {
			j = slices.Index(weekdays, to)
		}
		if i < 0 || j < 0 {
			return nil, fmt.Errorf("invalid window %q: unknown day in %q (want mon, tue, wed, thu, fri, sat, sun or a range like mon-fri)", s, item)
		}
		if isRange && i == j {
			return nil, fmt.Errorf("invalid window %q: day range %q starts and ends on the same day", s, item)
		}
		for d := i; ; d = (d + 1) % 7 {
			if seen[d] {
				return nil, fmt.Errorf("invalid window %q: %s is listed twice", s, weekdays[d])
			}
			seen[d] = true
			if d == j {
				break
			}
		}
	}
	fromT, toT, ok := strings.Cut(timesPart, "-")
	if !ok {
		return nil, fmt.Errorf("invalid window %q: want a time range HH:MM-HH:MM", s)
	}
	fm, err := parseClock(fromT, false)
	if err != nil {
		return nil, fmt.Errorf("invalid window %q: %w", s, err)
	}
	tm, err := parseClock(toT, true)
	if err != nil {
		return nil, fmt.Errorf("invalid window %q: %w", s, err)
	}
	if fm == tm {
		return nil, fmt.Errorf("invalid window %q: the window is empty (from equals to)", s)
	}
	w := &mycelv1.ScheduleWindow{From: fromT, To: toT}
	for d := range weekdays {
		if seen[d] {
			w.Days = append(w.Days, weekdays[d])
		}
	}
	return w, nil
}

// parseClock parses "HH:MM" (00:00-23:59, plus 24:00 when allow24) into
// minutes after midnight.
func parseClock(s string, allow24 bool) (int, error) {
	if len(s) != 5 || s[2] != ':' || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[3]) || !isDigit(s[4]) {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is not HH:MM", s)
	}
	if h == 24 && m == 0 {
		if !allow24 {
			return 0, fmt.Errorf("time %q: a window cannot start at 24:00", s)
		}
		return 24 * 60, nil
	}
	if h > 23 {
		return 0, fmt.Errorf("time %q is out of range", s)
	}
	return h*60 + m, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// windowText renders a window as "mon,tue=22:00-07:00".
func windowText(w *mycelv1.ScheduleWindow) string {
	return strings.Join(w.GetDays(), ",") + "=" + w.GetFrom() + "-" + w.GetTo()
}

// shareFlags are the flags of `nodes share` and `node share`.
type shareFlags struct {
	offer      []string
	when       string
	windows    []string
	localFirst bool
	maxJobs    int32
	idleAfter  time.Duration
}

func addShareFlags(cmd *cobra.Command, f *shareFlags) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.offer, "offer", nil, "resources offered, comma-separated: docker, ollama, gpu, sandbox (--offer '' offers nothing)")
	fl.StringVar(&f.when, "when", "", "when to share: always, idle or schedule")
	fl.StringArrayVar(&f.windows, "window", nil, "schedule window days=HH:MM-HH:MM, e.g. mon-fri=22:00-07:00 or sat,sun=00:00-24:00 (repeatable; replaces the schedule; implies --when schedule)")
	fl.BoolVar(&f.localFirst, "local-first", false, "throttle or pause remote work while the owner is active (--local-first=false to turn off)")
	fl.Int32Var(&f.maxJobs, "max-jobs", 0, "concurrent remote jobs (0 = default)")
	fl.DurationVar(&f.idleAfter, "idle-after", 0, "for --when idle: how long the machine must be idle, whole minutes, e.g. 10m")
}

// shareHelp documents the sharing flags' grammar and the read-modify-write
// semantics in the commands' Long text.
const shareHelp = "\n\nWindows: --window days=HH:MM-HH:MM in the node's local time. days is a list of mon, tue, wed,\n" +
	"thu, fri, sat, sun and ranges, e.g. mon-fri or sat,sun (a range may wrap: fri-mon). Times are 24-hour\n" +
	"HH:MM; the end may be 24:00, the start may not; an end earlier than the start runs past midnight\n" +
	"(mon-fri=22:00-07:00). --window replaces the whole schedule and implies --when schedule; any other\n" +
	"--when clears the schedule.\n\n" +
	"Read-modify-write: the current settings are read, the flags given are applied and the result is\n" +
	"written back. A change made by someone else between the read and the write is overwritten."

var shareFlagNames = []string{"offer", "when", "window", "local-first", "max-jobs", "idle-after"}

func anyChanged(cmd *cobra.Command, names []string) bool {
	return slices.ContainsFunc(names, cmd.Flags().Changed)
}

// applyShare returns a copy of base with the given flags applied; settings
// whose flags were not given keep their current value (read-modify-write).
func applyShare(base *mycelv1.SharingSettings, f *shareFlags, changed func(string) bool) (*mycelv1.SharingSettings, error) {
	s := &mycelv1.SharingSettings{}
	if base != nil {
		s, _ = proto.Clone(base).(*mycelv1.SharingSettings)
	}
	if changed("offer") {
		names := []string{}
		for _, n := range splitList(f.offer) {
			names = append(names, strings.ToLower(n))
		}
		s.Offer = &mycelv1.Capabilities{Names: names}
	}
	if changed("when") {
		w := strings.ToLower(strings.TrimSpace(f.when))
		switch w {
		case "always", "idle", "schedule":
		default:
			return nil, usagef("invalid --when %q: want always, idle or schedule", f.when)
		}
		if changed("window") && w != "schedule" {
			return nil, usagef("--window needs --when schedule (got --when %s)", w)
		}
		s.When = w
	}
	if changed("window") {
		s.Schedule = nil
		for _, v := range f.windows {
			w, err := parseWindow(v)
			if err != nil {
				return nil, usageError{err}
			}
			s.Schedule = append(s.Schedule, w)
		}
		if !changed("when") {
			s.When = "schedule"
		}
	}
	if s.GetWhen() == "schedule" && len(s.GetSchedule()) == 0 {
		return nil, usagef("--when schedule needs at least one --window")
	}
	if s.GetWhen() != "schedule" {
		s.Schedule = nil // windows only apply to "schedule"
	}
	if changed("local-first") {
		s.LocalFirst = f.localFirst
	}
	if changed("max-jobs") {
		if f.maxJobs < 0 {
			return nil, usagef("--max-jobs must not be negative")
		}
		s.MaxJobs = f.maxJobs
	}
	if changed("idle-after") {
		if f.idleAfter < time.Minute || f.idleAfter%time.Minute != 0 || f.idleAfter > 24*time.Hour {
			return nil, usagef("--idle-after must be whole minutes between 1m and 24h, e.g. 10m")
		}
		s.IdleAfterMinutes = int32(f.idleAfter / time.Minute)
	}
	return s, nil
}

// sharingCells renders settings as table cells in the order of sharingLabels.
func sharingCells(s *mycelv1.SharingSettings) []string {
	if s == nil {
		return []string{"-", "-", "-", "-", "-", "-"}
	}
	offer := "-"
	if s.GetOffer() != nil {
		offer = "none"
		if len(s.GetOffer().GetNames()) > 0 {
			offer = strings.Join(s.GetOffer().GetNames(), ",")
		}
	}
	var wins []string
	for _, w := range s.GetSchedule() {
		wins = append(wins, windowText(w))
	}
	maxJobs, idle := "default", "default"
	if s.GetMaxJobs() > 0 {
		maxJobs = strconv.Itoa(int(s.GetMaxJobs()))
	}
	if s.GetIdleAfterMinutes() > 0 {
		idle = strconv.Itoa(int(s.GetIdleAfterMinutes())) + "m"
	}
	return []string{offer, dash(s.GetWhen()), dash(strings.Join(wins, " ")), yesNo(s.GetLocalFirst()), maxJobs, idle}
}

var sharingLabels = []string{"Offer", "When", "Schedule", "Local first", "Max jobs", "Idle after"}

// printSharing prints one column per settings set under the given headers.
func (a *app) printSharing(headers []string, sets ...*mycelv1.SharingSettings) error {
	tw := newTable(a.stdout)
	if len(headers) > 0 {
		row(tw, append([]string{"SETTING"}, headers...)...)
	}
	cells := make([][]string, len(sets))
	for i, s := range sets {
		cells[i] = sharingCells(s)
	}
	for r, label := range sharingLabels {
		cols := []string{label + ":"}
		if len(headers) > 0 {
			cols[0] = label
		}
		for i := range sets {
			cols = append(cols, cells[i][r])
		}
		row(tw, cols...)
	}
	return tw.Flush()
}

func (a *app) nodesShareCmd() *cobra.Command {
	var f shareFlags
	cmd := &cobra.Command{
		Use:   "share <name> [--offer a,b] [--when always|idle|schedule] [--window days=HH:MM-HH:MM]... [--local-first[=false]] [--max-jobs n] [--idle-after 10m]",
		Short: "Show or change what a node shares (hub-side settings, within its join token's ceiling)",
		Long: "Without flags, shows the node's hub-side, local and effective sharing settings. With flags, changes only\n" +
			"the settings given and keeps the others, starting from the hub-side settings." + shareHelp,
		Args: exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cur, err := hc.control.GetNodeSharing(ctx, connect.NewRequest(&mycelv1.GetNodeSharingRequest{Name: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			res := cur.Msg
			if anyChanged(cmd, shareFlagNames) {
				next, err := applyShare(cur.Msg.GetHub(), &f, cmd.Flags().Changed)
				if err != nil {
					return err
				}
				set, err := hc.control.SetNodeSharing(ctx, connect.NewRequest(&mycelv1.SetNodeSharingRequest{Name: args[0], Settings: next}))
				if err != nil {
					return rpcError(hc.url, err)
				}
				res = set.Msg.GetSharing()
			}
			if a.jsonOut {
				return a.printProto(res)
			}
			return a.printSharing([]string{"HUB", "LOCAL", "EFFECTIVE"}, res.GetHub(), res.GetLocal(), res.GetEffective())
		},
	}
	addShareFlags(cmd, &f)
	return cmd
}

// ---- this machine (local API) ----

func (a *app) nodeCmd() *cobra.Command {
	cmd := groupCmd("node", "This machine's sharing, grants and freeze state (local API; restrict only)")
	grants := groupCmd("grants", "Grants attached on this machine", "grant")
	grants.AddCommand(a.nodeGrantsLsCmd(), a.nodeGrantsRevokeCmd())
	freeze := groupCmd("freeze", "This machine's freeze state")
	freeze.AddCommand(a.nodeFreezeStatusCmd())
	cmd.AddCommand(a.nodeShareCmd(), grants, freeze)
	// `mycel node ls|get|rm|caps` keep working as they did when "node" was
	// an alias of "nodes".
	for _, c := range []*cobra.Command{a.nodesLsCmd(), a.nodesGetCmd(), a.nodesRmCmd(), a.nodesCapsCmd()} {
		c.Hidden = true
		cmd.AddCommand(c)
	}
	return cmd
}

func (a *app) nodeShareCmd() *cobra.Command {
	var f shareFlags
	var localAddr string
	cmd := &cobra.Command{
		Use:   "share [--offer a,b] [--when always|idle|schedule] [--window days=HH:MM-HH:MM]... [--local-first[=false]] [--max-jobs n] [--idle-after 10m]",
		Short: "Show or tighten what this machine shares (a local ceiling the hub cannot exceed)",
		Long: "Without flags, shows this machine's hub-side, local and effective sharing settings. With flags, changes only\n" +
			"the settings given, starting from this machine's local ceiling when one is set and otherwise from the\n" +
			"effective settings, and stores the result as the local ceiling. The local ceiling may only tighten the\n" +
			"hub-side settings; widening is done from the hub (mycel nodes share)." + shareHelp,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cur, err := c.Sharing(ctx)
			if err != nil {
				return err
			}
			if !anyChanged(cmd, shareFlagNames) {
				if a.jsonOut {
					return a.printProto(cur)
				}
				return a.printSharing([]string{"HUB", "LOCAL", "EFFECTIVE"}, cur.GetHub(), cur.GetLocal(), cur.GetEffective())
			}
			base := cur.GetEffective()
			if l := cur.GetLocal(); l != nil && proto.Size(l) > 0 {
				base = l
			}
			next, err := applyShare(base, &f, cmd.Flags().Changed)
			if err != nil {
				return err
			}
			eff, err := c.SetSharing(ctx, next)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printProto(eff)
			}
			if _, err := fmt.Fprintln(a.stdout, "This machine now shares:"); err != nil {
				return err
			}
			return a.printSharing(nil, eff)
		},
	}
	addShareFlags(cmd, &f)
	localAddrFlag(cmd, &localAddr)
	return cmd
}

func (a *app) nodeGrantsLsCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the grants attached on this machine",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			gs, err := c.LocalGrants(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printProto(&mycelv1.ListGrantsResponse{Grants: gs})
			}
			return a.printGrantsTable(gs)
		},
	}
	localAddrFlag(cmd, &localAddr)
	return cmd
}

func (a *app) nodeGrantsRevokeCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a grant on this machine; its broker detaches it at once and the hub is told",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			if err := c.RevokeLocalGrant(cmd.Context(), args[0]); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]string{"revoked": args[0]})
			}
			_, err = fmt.Fprintf(a.stdout, "Revoked %s on this machine.\n", cellText(args[0]))
			return err
		},
	}
	localAddrFlag(cmd, &localAddr)
	return cmd
}

func (a *app) nodeFreezeStatusCmd() *cobra.Command {
	var localAddr string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether this machine is frozen",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.localClient(cmd, localAddr)
			if err != nil {
				return err
			}
			st, err := c.Freeze(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSONValue(st)
			}
			msg := "This machine is not frozen."
			if st.Frozen {
				msg = "This machine is frozen (scope " + dash(st.Scope) + ")"
				if st.Reason != "" {
					msg += ": " + st.Reason
				}
				msg += "."
			}
			_, err = fmt.Fprintln(a.stdout, cellText(msg))
			return err
		},
	}
	localAddrFlag(cmd, &localAddr)
	return cmd
}
