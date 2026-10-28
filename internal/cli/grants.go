package cli

// Phase 5: grants and approvals.
//
//	mycel grants ls [--pending] [--node n] [--subject s] [--all]
//	mycel grants get <id>
//	mycel grants request --kind … --subject … [--duration] [--reason] [kind flags]
//	mycel approve <id> [--duration d] [--note n]   (also: mycel grants approve)
//	mycel deny <id> [--note n]                     (also: mycel grants deny)
//	mycel grants revoke <id> [--note n]

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	mcpadapter "github.com/richard-sen27/mycelium/internal/adapters/mcp"
)

func (a *app) grantsCmd() *cobra.Command {
	cmd := groupCmd("grants", "List, request, approve and revoke resource grants", "grant")
	cmd.AddCommand(a.grantsLsCmd(), a.grantsGetCmd(), a.grantsRequestCmd(),
		a.approveCmd(), a.denyCmd(), a.grantsRevokeCmd())
	return cmd
}

// normSubject accepts "agent:<name>@<node>", "<name>@<node>" or "job:<id>".
func normSubject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "job:") {
		return s
	}
	return normAgentID(s)
}

// until renders an RFC 3339 expiry relative to now: "in 1h59m", or "x ago"
// once it has passed.
func until(ts string, now time.Time) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := t.Sub(now)
	if d <= 0 {
		return ago(ts, now)
	}
	if d < time.Minute {
		return "in <1m"
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("in %dm", m)
	case m == 0:
		return fmt.Sprintf("in %dh", h)
	default:
		return fmt.Sprintf("in %dh%dm", h, m)
	}
}

func (a *app) printGrantsTable(gs []*mycelv1.Grant) error {
	now := a.now()
	tw := newTable(a.stdout)
	row(tw, "ID", "STATE", "RESOURCE", "SUBJECT", "NODE", "EXPIRES", "REASON")
	for _, g := range gs {
		exp := until(g.GetExpiresAt(), now)
		if g.GetExpiresAt() == "" && g.GetDuration() != "" {
			exp = "for " + g.GetDuration()
		}
		row(tw, g.GetId(), mcpadapter.GrantStateName(g.GetState()), mcpadapter.GrantSpecText(g.GetSpec()),
			dash(g.GetSubject()), dash(g.GetNode()), exp, shorten(dash(g.GetReason()), 60))
	}
	return tw.Flush()
}

func (a *app) grantsLsCmd() *cobra.Command {
	var pending, all bool
	var node, subject string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List grants (pending and active; --all adds finished ones)",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.ListGrantsRequest{PendingOnly: pending, Node: node, IncludeFinished: all}
			if subject != "" {
				req.Subject = normSubject(subject)
			}
			resp, err := hc.grants.ListGrants(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			return a.printGrantsTable(resp.Msg.GetGrants())
		},
	}
	f := cmd.Flags()
	f.BoolVar(&pending, "pending", false, "only grants waiting for an approval")
	f.StringVar(&node, "node", "", "only grants on this node")
	f.StringVar(&subject, "subject", "", "only grants held by this agent or job")
	f.BoolVar(&all, "all", false, "include expired, revoked and denied grants")
	return cmd
}

func (a *app) grantsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show one grant",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.grants.GetGrant(cmd.Context(), connect.NewRequest(&mycelv1.GetGrantRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			a.printGrant(resp.Msg.GetGrant())
			return nil
		},
	}
}

func (a *app) printGrant(g *mycelv1.Grant) {
	now := a.now()
	tw := newTable(a.stdout)
	kv := func(k, v string) { row(tw, k+":", v) }
	kv("ID", dash(g.GetId()))
	kv("State", mcpadapter.GrantStateName(g.GetState()))
	kv("Resource", mcpadapter.GrantSpecText(g.GetSpec()))
	kv("Subject", dash(g.GetSubject()))
	kv("Node", dash(g.GetNode()))
	kv("Owner", dash(g.GetOwnerId()))
	kv("Requested by", dash(g.GetRequestedBy()))
	kv("Reason", dash(g.GetReason()))
	kv("Duration", dash(g.GetDuration()))
	kv("Channel", dash(g.GetChannel()))
	kv("Needs approval", yesNo(g.GetRequiresApproval()))
	kv("Created", dash(g.GetCreatedAt()))
	kv("Activated", dash(g.GetActivatedAt()))
	if g.GetExpiresAt() != "" {
		kv("Expires", g.GetExpiresAt()+" ("+until(g.GetExpiresAt(), now)+")")
	} else {
		kv("Expires", "-")
	}
	kv("Decided by", dash(g.GetDecidedBy()))
	kv("Note", dash(g.GetDecisionNote()))
	_ = tw.Flush()
}

// grantRequestFlags are the flags of `grants request`.
type grantRequestFlags struct {
	kind, subject, duration, reason, channel string
	models                                   []string
	allowPull                                bool
	device                                   int32
	exclusive                                bool
	images                                   []string
	hostPath, guestPath                      string
	readWrite                                bool
	allow                                    []string
}

// kindFlags lists the flags that belong to each kind.
var kindFlags = map[string][]string{
	mcpadapter.KindOllama:     {"model", "allow-pull"},
	mcpadapter.KindGPU:        {"device", "exclusive"},
	mcpadapter.KindDockerHost: {"image"},
	mcpadapter.KindMount:      {"host-path", "guest-path", "rw"},
	mcpadapter.KindEgress:     {"allow"},
}

// buildGrantSpec checks the kind-specific flags (changed reports which were
// given) and returns the spec.
func buildGrantSpec(f *grantRequestFlags, changed func(string) bool) (*mycelv1.GrantSpec, error) {
	spec, err := buildGrantSpecFlags(f, changed)
	if err != nil {
		return nil, err
	}
	if err := mcpadapter.ValidateGrantSpec(spec); err != nil {
		return nil, usageError{err}
	}
	return spec, nil
}

func buildGrantSpecFlags(f *grantRequestFlags, changed func(string) bool) (*mycelv1.GrantSpec, error) {
	own, ok := kindFlags[f.kind]
	if !ok {
		return nil, usagef("--kind must be one of %s", strings.Join(mcpadapter.GrantKinds, ", "))
	}
	for k, flags := range kindFlags {
		if k == f.kind {
			continue
		}
		for _, fl := range flags {
			if changed(fl) {
				return nil, usagef("--%s does not apply to a %s grant (its flags: --%s)", fl, f.kind, strings.Join(own, ", --"))
			}
		}
	}
	switch f.kind {
	case mcpadapter.KindOllama:
		return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{
			Models: splitList(f.models), AllowPull: f.allowPull}}}, nil
	case mcpadapter.KindGPU:
		if f.device < 0 {
			return nil, usagef("--device must not be negative")
		}
		return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Gpu{Gpu: &mycelv1.GpuGrant{Device: f.device, Exclusive: f.exclusive}}}, nil
	case mcpadapter.KindDockerHost:
		imgs := splitList(f.images)
		if len(imgs) == 0 {
			return nil, usagef("a docker-host grant needs at least one --image")
		}
		return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{Images: imgs}}}, nil
	case mcpadapter.KindMount:
		if strings.TrimSpace(f.hostPath) == "" || strings.TrimSpace(f.guestPath) == "" {
			return nil, usagef("a mount grant needs --host-path and --guest-path")
		}
		return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{
			HostPath: f.hostPath, GuestPath: f.guestPath, ReadOnly: !f.readWrite}}}, nil
	default: // egress
		allow := splitList(f.allow)
		if len(allow) == 0 {
			return nil, usagef("an egress grant needs at least one --allow")
		}
		return &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: allow}}}, nil
	}
}

func (a *app) grantsRequestCmd() *cobra.Command {
	var f grantRequestFlags
	cmd := &cobra.Command{
		Use:   "request --kind ollama|gpu|docker-host|mount|egress --subject agent:… [flags]",
		Short: "Request a grant for an agent or job you own",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(f.subject) == "" {
				return usagef("--subject is required (agent:<name>@<node> or job:<id>)")
			}
			spec, err := buildGrantSpec(&f, cmd.Flags().Changed)
			if err != nil {
				return err
			}
			if err := mcpadapter.CheckGrantDuration(f.duration); err != nil {
				return usageError{err}
			}
			if err := mcpadapter.CheckGrantReason(f.reason); err != nil {
				return usageError{err}
			}
			ch, err := mcpadapter.CheckGrantChannel(f.channel)
			if err != nil {
				return usageError{err}
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.RequestGrantRequest{Spec: spec, Subject: normSubject(f.subject), Duration: f.duration, Reason: f.reason, Channel: ch}
			resp, err := hc.grants.RequestGrant(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if resp.Msg.GetGrant().GetId() == "" {
				return mcpadapter.ErrNoGrant
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			g := resp.Msg.GetGrant()
			var msg string
			switch g.GetState() {
			case mycelv1.GrantState_GRANT_STATE_ACTIVE:
				msg = fmt.Sprintf("Grant %s is active until %s (%s on %s).", g.GetId(), dash(g.GetExpiresAt()), mcpadapter.GrantSpecText(g.GetSpec()), dash(g.GetNode()))
			case mycelv1.GrantState_GRANT_STATE_PENDING:
				msg = fmt.Sprintf("Grant %s is pending approval: `mycel approve %s` or `mycel deny %s`.", g.GetId(), g.GetId(), g.GetId())
			default:
				msg = fmt.Sprintf("Grant %s is %s.", g.GetId(), mcpadapter.GrantStateName(g.GetState()))
			}
			_, err = fmt.Fprintln(a.stdout, cellText(msg))
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.kind, "kind", "", "resource kind: ollama, gpu, docker-host, mount or egress")
	fl.StringVar(&f.subject, "subject", "", "who holds the grant: agent:<name>@<node> or job:<id>")
	fl.StringVar(&f.duration, "duration", "", "how long, a Go duration (hub default 1h, max 24h)")
	fl.StringVar(&f.reason, "reason", "", "why, shown to the approver")
	fl.StringVar(&f.channel, "channel", "", "channel the work belongs to (policy context)")
	fl.StringSliceVar(&f.models, "model", nil, "ollama: allowed model (repeatable; none = every model already present)")
	fl.BoolVar(&f.allowPull, "allow-pull", false, "ollama: allow pulling models")
	fl.Int32Var(&f.device, "device", 0, "gpu: device index")
	fl.BoolVar(&f.exclusive, "exclusive", false, "gpu: exclusive lease instead of time-sliced")
	fl.StringSliceVar(&f.images, "image", nil, "docker-host: allowed image or registry prefix (repeatable)")
	fl.StringVar(&f.hostPath, "host-path", "", "mount: directory on the node")
	fl.StringVar(&f.guestPath, "guest-path", "", "mount: path inside the sandbox")
	fl.BoolVar(&f.readWrite, "rw", false, "mount: read-write (mounts are read-only by default)")
	fl.StringSliceVar(&f.allow, "allow", nil, "egress: additional host rule, e.g. pypi.org:443 (repeatable)")
	return cmd
}

func (a *app) approveCmd() *cobra.Command {
	var duration, note string
	cmd := &cobra.Command{
		Use:   "approve <id> [--duration d] [--note n]",
		Short: "Approve a pending grant (admin or owner of its node)",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := mcpadapter.CheckGrantDuration(duration); err != nil {
				return usageError{err}
			}
			if err := mcpadapter.CheckGrantReason(note); err != nil {
				return usageError{err}
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.grants.ApproveGrant(cmd.Context(), connect.NewRequest(&mycelv1.ApproveGrantRequest{Id: args[0], Duration: duration, Note: note}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if resp.Msg.GetGrant().GetId() == "" {
				return mcpadapter.ErrNoGrant
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			g := resp.Msg.GetGrant()
			msg := fmt.Sprintf("Approved %s: %s", args[0], mcpadapter.GrantStateName(g.GetState()))
			if g.GetExpiresAt() != "" {
				msg += " until " + g.GetExpiresAt()
			}
			_, err = fmt.Fprintln(a.stdout, cellText(msg+"."))
			return err
		},
	}
	cmd.Flags().StringVar(&duration, "duration", "", "shorten the requested duration")
	cmd.Flags().StringVar(&note, "note", "", "note sent to the requester with the decision")
	return cmd
}

func (a *app) denyCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "deny <id> [--note n]",
		Short: "Deny a pending grant",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.grants.DenyGrant(cmd.Context(), connect.NewRequest(&mycelv1.DenyGrantRequest{Id: args[0], Note: note}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if resp.Msg.GetGrant().GetId() == "" {
				return mcpadapter.ErrNoGrant
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Denied %s.\n", cellText(args[0]))
			return err
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "note sent to the requester with the decision")
	return cmd
}

func (a *app) grantsRevokeCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "revoke <id> [--note n]",
		Short: "Revoke a grant; its node detaches it at once",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.grants.RevokeGrant(cmd.Context(), connect.NewRequest(&mycelv1.RevokeGrantRequest{Id: args[0], Note: note}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if resp.Msg.GetGrant().GetId() == "" {
				return mcpadapter.ErrNoGrant
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Revoked %s.\n", cellText(args[0]))
			return err
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "why (recorded with the grant)")
	return cmd
}
