// Package channels holds the channel domain rules that do not need storage:
// names, direct-message naming, policy defaults, hop and thread computation,
// subscription matching, per-agent rate limiting and the late-joiner summary.
// The hub package stores channels and messages and calls into this package.
//
// Contract (owned by the orchestrator): exported names and documented
// semantics are fixed; the implementing agent fills in bodies and may add
// helpers.
//
// Rules (spec: "Channels", "Message envelope and bus", "Security, policy and control"):
//   - Topic, task and system channel names are "#" + [a-z0-9-]{1,63}, not
//     starting or ending with "-". Task channels start with "#task-". System
//     channels are "#alerts", "#approvals" and "#node-<name>".
//   - A direct message channel is "dm:<id1>:<id2>" with the two participant
//     ids sorted; participants are agent ids ("agent:<name>@<node>") or user
//     ids ("user:<id>").
//   - hop: a message without reply_to has hop 0; a reply has parent.hop+1. A
//     post whose hop would exceed the channel's max_hops is refused.
//   - thread: a reply inherits the parent's thread, or the parent's id when
//     the parent has none; a non-reply with an explicit thread keeps it.
//   - Delivery to an agent's inbox: the agent is a channel member, is not the
//     sender, and its subscription for that channel matches (mentions_only →
//     the agent id is in `to`; types and tags filters when non-empty).
//   - Rate limit: a token bucket per (agent, channel) with rate_per_minute
//     tokens per minute and the same burst.
//   - Summary for late joiners (no model in v1): a plain-text digest with the
//     channel name, type, member count, the blackboard keys and values
//     (truncated), and the last N messages as "[ts] from: text".
package channels

import (
	"errors"
	"strconv"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("channels: not implemented")

// Defaults applied where ChannelPolicy has zero values.
const (
	DefaultMaxHops       = 8
	DefaultRatePerMinute = 60
	MaxTextBytes         = 64 * 1024
	MaxBlackboardValue   = 64 * 1024
	MaxMentions          = 32
	SummaryMessages      = 20
)

// Well-known channels.
const (
	General   = "#general"
	Alerts    = "#alerts"
	Approvals = "#approvals"
)

// ValidName reports whether name is a valid topic/task/system channel name.
func ValidName(name string) bool { panic(ErrNotImplemented) }

// IsTask reports whether name is a task channel ("#task-...").
func IsTask(name string) bool { panic(ErrNotImplemented) }

// IsSystem reports whether name is a system channel.
func IsSystem(name string) bool { panic(ErrNotImplemented) }

// IsDirect reports whether name is a direct-message channel ("dm:...").
func IsDirect(name string) bool { panic(ErrNotImplemented) }

// DirectName returns the canonical direct channel name for two participants.
func DirectName(a, b string) string { panic(ErrNotImplemented) }

// DirectParticipants splits a direct channel name.
func DirectParticipants(name string) (string, string, bool) { panic(ErrNotImplemented) }

// NodeChannel returns "#node-<name>".
func NodeChannel(node string) string { panic(ErrNotImplemented) }

// NewMessageID returns "msg_" + a 26-char Crockford base32 ULID (time-ordered,
// random suffix from crypto/rand).
func NewMessageID(now time.Time) string { panic(ErrNotImplemented) }

// NewThreadID returns "thr_" + ULID.
func NewThreadID(now time.Time) string { panic(ErrNotImplemented) }

// WithDefaults returns a copy of p with zero values replaced by defaults and
// the system-channel rule applied (agents never post on system channels).
func WithDefaults(p *mycelv1.ChannelPolicy, typ mycelv1.ChannelType) *mycelv1.ChannelPolicy {
	panic(ErrNotImplemented)
}

// Hop returns the hop for a new message given its parent (nil when not a
// reply) and reports whether it exceeds maxHops.
func Hop(parent *mycelv1.Message, maxHops int32) (hop int32, exceeded bool) { panic(ErrNotImplemented) }

// Thread returns the thread id for a new message: explicit wins, else the
// parent's thread, else the parent's id, else "".
func Thread(explicit string, parent *mycelv1.Message) string { panic(ErrNotImplemented) }

// Matches reports whether msg should be delivered to agentID under sub.
// sub may be nil (deliver everything). The caller has already checked
// membership and that agentID is not the sender.
func Matches(sub *mycelv1.Subscription, msg *mycelv1.Message, agentID string) bool {
	panic(ErrNotImplemented)
}

// RateLimiter is a token bucket per (agent, channel). Safe for concurrent use.
type RateLimiter struct {
	// unexported
}

// NewRateLimiter creates a limiter; now is injectable for tests.
func NewRateLimiter(now func() time.Time) *RateLimiter { panic(ErrNotImplemented) }

// Allow consumes one token for (agent, channel) at ratePerMinute; false when empty.
func (r *RateLimiter) Allow(agent, channel string, ratePerMinute int32) bool {
	panic(ErrNotImplemented)
}

// Summary builds the late-joiner digest.
func Summary(ch *mycelv1.Channel, bb []*mycelv1.BlackboardEntry, recent []*mycelv1.Message) string {
	panic(ErrNotImplemented)
}

// QuoteForAgent renders a message as quoted, clearly untrusted data for an
// adapter to hand to a harness:
//
//	[mycel message msg_… in #backend from agent:coder@mac (untrusted) reply_to msg_… hop 2 at 2026-10-28T14:03:11Z]
//	<text, each line prefixed with "> ">
//	[end mycel message]
func QuoteForAgent(msg *mycelv1.Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("[mycel message ")
	b.WriteString(msg.GetId())
	b.WriteString(" in ")
	b.WriteString(msg.GetChannel())
	b.WriteString(" from ")
	b.WriteString(msg.GetFrom())
	if msg.GetMeta().GetProvenance().GetUntrusted() {
		b.WriteString(" (untrusted)")
	}
	if t := msg.GetType(); t != mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED && t != mycelv1.MessageType_MESSAGE_TYPE_CHAT {
		b.WriteString(" type ")
		b.WriteString(strings.ToLower(strings.TrimPrefix(t.String(), "MESSAGE_TYPE_")))
	}
	if th := msg.GetThread(); th != "" {
		b.WriteString(" thread ")
		b.WriteString(th)
	}
	if r := msg.GetMeta().GetReplyTo(); r != "" {
		b.WriteString(" reply_to ")
		b.WriteString(r)
	}
	if len(msg.GetTo()) > 0 {
		b.WriteString(" to ")
		b.WriteString(strings.Join(msg.GetTo(), ","))
	}
	b.WriteString(" hop ")
	b.WriteString(strconv.Itoa(int(msg.GetMeta().GetHop())))
	b.WriteString(" seq ")
	b.WriteString(strconv.FormatUint(msg.GetSeq(), 10))
	b.WriteString(" at ")
	b.WriteString(msg.GetTs())
	b.WriteString("]\n")
	for _, line := range strings.Split(strings.TrimRight(msg.GetPayload().GetText(), "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, p := range msg.GetPayload().GetParts() {
		b.WriteString("> [part ")
		b.WriteString(p.GetKind())
		b.WriteString(" ")
		b.WriteString(p.GetRef())
		if p.GetName() != "" {
			b.WriteString(" ")
			b.WriteString(p.GetName())
		}
		b.WriteString("]\n")
	}
	b.WriteString("[end mycel message]")
	return b.String()
}
