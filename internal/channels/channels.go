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
//   - thread: a reply always inherits the parent's thread, or the parent's
//     id when the parent has none; a non-reply with an explicit thread keeps
//     it.
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
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

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
func ValidName(name string) bool {
	rest, ok := strings.CutPrefix(name, "#")
	return ok && validLabel(rest)
}

// validLabel is the node-name rule: 1-63 chars of [a-z0-9-], not starting or
// ending with '-'.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// IsTask reports whether name is a task channel ("#task-...").
func IsTask(name string) bool { return ValidName(name) && strings.HasPrefix(name, "#task-") }

// IsSystem reports whether name is a system channel.
func IsSystem(name string) bool {
	if !ValidName(name) {
		return false
	}
	return name == Alerts || name == Approvals || strings.HasPrefix(name, "#node-")
}

// IsDirect reports whether name is a direct-message channel ("dm:...").
func IsDirect(name string) bool {
	_, _, ok := DirectParticipants(name)
	return ok
}

// DirectName returns the canonical direct channel name for two participants.
func DirectName(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}

// DirectParticipants splits a direct channel name. Only canonical names
// (two distinct, well-formed participants in sorted order) are accepted.
func DirectParticipants(name string) (string, string, bool) {
	rest, ok := strings.CutPrefix(name, "dm:")
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 4 {
		return "", "", false
	}
	a, b := parts[0]+":"+parts[1], parts[2]+":"+parts[3]
	if !ValidParticipant(a) || !ValidParticipant(b) || a >= b {
		return "", "", false
	}
	return a, b, true
}

// ValidParticipant reports whether id is a well-formed agent id
// ("agent:<name>@<node>") or user id ("user:<id>") as used in direct channel
// names and mentions. It does not check that the participant exists.
func ValidParticipant(id string) bool {
	kind, rest, ok := strings.Cut(id, ":")
	if !ok || rest == "" || len(rest) > 128 {
		return false
	}
	switch kind {
	case "agent":
		name, node, ok := strings.Cut(rest, "@")
		return ok && validLabel(name) && validLabel(node)
	case "user":
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// NodeChannel returns "#node-<name>".
func NodeChannel(node string) string { return "#node-" + node }

// crockford is the Crockford base32 alphabet used by ULIDs.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a 26-char ULID: 10 chars of millisecond time and 16 chars
// (80 bits) of randomness from crypto/rand.
func newULID(now time.Time) string {
	var out [26]byte
	ms := uint64(now.UnixMilli()) & (1<<48 - 1) //nolint:gosec // masked to the 48-bit ULID time field
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&31]
		ms >>= 5
	}
	var rnd [10]byte
	_, _ = rand.Read(rnd[:]) // crypto/rand.Read never returns an error
	// 80 bits -> 16 groups of 5 bits, most significant first.
	for i := range 16 {
		var v byte
		for j := range 5 {
			bit := i*5 + j
			v = v<<1 | (rnd[bit/8]>>(7-bit%8))&1
		}
		out[10+i] = crockford[v]
	}
	return string(out[:])
}

// NewMessageID returns "msg_" + a 26-char Crockford base32 ULID (time-ordered,
// random suffix from crypto/rand).
func NewMessageID(now time.Time) string { return "msg_" + newULID(now) }

// NewThreadID returns "thr_" + ULID.
func NewThreadID(now time.Time) string { return "thr_" + newULID(now) }

// ValidMessageID reports whether id has the NewMessageID shape.
func ValidMessageID(id string) bool { return validPrefixedULID(id, "msg_") }

// ValidThreadID reports whether id is a usable thread id: a NewThreadID id
// or a message id (a reply to a message without a thread starts a thread
// named after that message).
func ValidThreadID(id string) bool {
	return validPrefixedULID(id, "thr_") || validPrefixedULID(id, "msg_")
}

func validPrefixedULID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || len(rest) != 26 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if !strings.ContainsRune(crockford, rune(rest[i])) {
			return false
		}
	}
	return true
}

// WithDefaults returns a copy of p with zero values replaced by defaults and
// the system-channel rule applied (agents never post on system channels).
//
// proto3 booleans cannot be told apart from "unset", so a policy in which
// neither agents nor humans may post is read as unset and both are allowed;
// any other combination is kept. System channels allow neither: the hub
// posts there, humans and agents read. Negative limits count as unset.
func WithDefaults(p *mycelv1.ChannelPolicy, typ mycelv1.ChannelType) *mycelv1.ChannelPolicy {
	out := &mycelv1.ChannelPolicy{}
	if p != nil {
		out = proto.CloneOf(p)
	}
	if out.MaxHops <= 0 {
		out.MaxHops = DefaultMaxHops
	}
	if out.RatePerMinute <= 0 {
		out.RatePerMinute = DefaultRatePerMinute
	}
	if !out.AgentsMayPost && !out.HumansMayPost {
		out.AgentsMayPost, out.HumansMayPost = true, true
	}
	if typ == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM {
		out.AgentsMayPost, out.HumansMayPost = false, false
	}
	return out
}

// Hop returns the hop for a new message given its parent (nil when not a
// reply) and reports whether it exceeds maxHops.
func Hop(parent *mycelv1.Message, maxHops int32) (hop int32, exceeded bool) {
	if maxHops <= 0 {
		maxHops = DefaultMaxHops
	}
	if parent == nil {
		return 0, false
	}
	hop = parent.GetMeta().GetHop() + 1
	return hop, hop > maxHops
}

// Thread returns the thread id for a new message: a reply inherits the
// parent's thread, or the parent's id when the parent has none (explicit is
// ignored; the hub refuses a reply whose explicit thread differs); a
// non-reply keeps explicit, which may be "".
func Thread(explicit string, parent *mycelv1.Message) string {
	switch {
	case parent == nil:
		return explicit
	case parent.GetThread() != "":
		return parent.GetThread()
	default:
		return parent.GetId()
	}
}

// Matches reports whether msg should be delivered to agentID under sub.
// sub may be nil (deliver everything). The caller has already checked
// membership and that agentID is not the sender.
func Matches(sub *mycelv1.Subscription, msg *mycelv1.Message, agentID string) bool {
	if msg == nil {
		return false
	}
	if sub == nil {
		return true
	}
	if sub.GetMentionsOnly() && !slices.Contains(msg.GetTo(), agentID) {
		return false
	}
	if types := sub.GetTypes(); len(types) > 0 {
		t := msg.GetType()
		if t == mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED {
			t = mycelv1.MessageType_MESSAGE_TYPE_CHAT
		}
		if !slices.Contains(types, t) {
			return false
		}
	}
	if tags := sub.GetTags(); len(tags) > 0 {
		if !slices.ContainsFunc(msg.GetPayload().GetTags(), func(t string) bool { return slices.Contains(tags, t) }) {
			return false
		}
	}
	return true
}

// RateLimiter is a token bucket per (agent, channel). Safe for concurrent use.
type RateLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	buckets   map[rateKey]*rateBucket
	lastPrune time.Time
}

type rateKey struct{ agent, channel string }

type rateBucket struct {
	tokens float64
	rate   float64 // tokens per minute, also the burst
	last   time.Time
}

// NewRateLimiter creates a limiter; now is injectable for tests.
func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{now: now, buckets: map[rateKey]*rateBucket{}}
}

// Allow consumes one token for (agent, channel) at ratePerMinute; false when empty.
func (r *RateLimiter) Allow(agent, channel string, ratePerMinute int32) bool {
	if ratePerMinute <= 0 {
		ratePerMinute = DefaultRatePerMinute
	}
	rate := float64(ratePerMinute)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.lastPrune) > time.Minute {
		r.prune(now)
	}
	k := rateKey{agent, channel}
	b, ok := r.buckets[k]
	if !ok {
		b = &rateBucket{tokens: rate, rate: rate, last: now}
		r.buckets[k] = b
	}
	b.rate = rate // the channel policy may have changed
	if el := now.Sub(b.last); el > 0 {
		b.tokens += el.Minutes() * rate
		b.last = now
	}
	b.tokens = min(b.tokens, rate)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets that are full again; they carry no state.
func (r *RateLimiter) prune(now time.Time) {
	r.lastPrune = now
	for k, b := range r.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*b.rate >= b.rate {
			delete(r.buckets, k)
		}
	}
}

// Summary truncation limits.
const (
	summaryValueMax = 200
	summaryTextMax  = 400
)

// Summary builds the late-joiner digest.
func Summary(ch *mycelv1.Channel, bb []*mycelv1.BlackboardEntry, recent []*mycelv1.Message) string {
	var b strings.Builder
	typ := strings.ToLower(strings.TrimPrefix(ch.GetType().String(), "CHANNEL_TYPE_"))
	fmt.Fprintf(&b, "Channel %s (%s), %d members.\n", ch.GetName(), typ, len(ch.GetMembers()))
	if ch.GetArchivedAt() != "" {
		fmt.Fprintf(&b, "Archived at %s.\n", ch.GetArchivedAt())
	}
	if len(bb) == 0 {
		b.WriteString("Blackboard: empty.\n")
	} else {
		b.WriteString("Blackboard:\n")
		for _, e := range bb {
			fmt.Fprintf(&b, "  %s = %s\n", oneLine(e.GetKey(), summaryValueMax), oneLine(e.GetValue(), summaryValueMax))
		}
	}
	if len(recent) > SummaryMessages {
		recent = recent[len(recent)-SummaryMessages:]
	}
	if len(recent) == 0 {
		b.WriteString("No messages yet.\n")
	} else {
		fmt.Fprintf(&b, "Last %d messages:\n", len(recent))
		for _, m := range recent {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", SanitizeLine(m.GetTs()), SanitizeLine(m.GetFrom()), oneLine(m.GetPayload().GetText(), summaryTextMax))
		}
	}
	return b.String()
}

// oneLine collapses whitespace runs (newlines included) to single spaces and
// truncates to maxBytes on a rune boundary, adding "…" when cut.
func oneLine(s string, maxBytes int) string {
	s = SanitizeLine(strings.Join(strings.Fields(s), " "))
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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
	b.WriteString(SanitizeLine(msg.GetId()))
	b.WriteString(" in ")
	b.WriteString(SanitizeLine(msg.GetChannel()))
	b.WriteString(" from ")
	b.WriteString(SanitizeLine(msg.GetFrom()))
	if msg.GetMeta().GetProvenance().GetUntrusted() {
		b.WriteString(" (untrusted)")
	}
	if t := msg.GetType(); t != mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED && t != mycelv1.MessageType_MESSAGE_TYPE_CHAT {
		b.WriteString(" type ")
		b.WriteString(strings.ToLower(strings.TrimPrefix(t.String(), "MESSAGE_TYPE_")))
	}
	if th := msg.GetThread(); th != "" {
		b.WriteString(" thread ")
		b.WriteString(SanitizeLine(th))
	}
	if r := msg.GetMeta().GetReplyTo(); r != "" {
		b.WriteString(" reply_to ")
		b.WriteString(SanitizeLine(r))
	}
	if len(msg.GetTo()) > 0 {
		b.WriteString(" to ")
		b.WriteString(SanitizeLine(strings.Join(msg.GetTo(), ",")))
	}
	b.WriteString(" hop ")
	b.WriteString(strconv.Itoa(int(msg.GetMeta().GetHop())))
	b.WriteString(" seq ")
	b.WriteString(strconv.FormatUint(msg.GetSeq(), 10))
	b.WriteString(" at ")
	b.WriteString(SanitizeLine(msg.GetTs()))
	b.WriteString("]\n")
	for _, line := range strings.Split(strings.TrimRight(normalizeNewlines(msg.GetPayload().GetText()), "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, p := range msg.GetPayload().GetParts() {
		b.WriteString("> [part ")
		b.WriteString(SanitizeLine(p.GetKind()))
		b.WriteString(" ")
		b.WriteString(SanitizeLine(p.GetRef()))
		if p.GetName() != "" {
			b.WriteString(" ")
			b.WriteString(SanitizeLine(p.GetName()))
		}
		b.WriteString("]\n")
	}
	b.WriteString("[end mycel message]")
	return b.String()
}

// isLineBreakingRune reports runes that terminals or models may treat as a
// line break or that hide text: C0 and C1 controls (U+0085 included), DEL,
// U+2028 and U+2029.
func isLineBreakingRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// IsBidiControl reports the bidirectional embedding, override and isolate
// controls (U+202A\u2013U+202E, U+2066\u2013U+2069), which can reorder how text is
// displayed.
func IsBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// IsPrintable reports whether s is valid UTF-8 without control characters
// (C0, C1, DEL), line or paragraph separators, or bidi controls.
func IsPrintable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if isLineBreakingRune(r) || IsBidiControl(r) {
			return false
		}
	}
	return true
}

// SanitizeLine makes s safe to print inside a one-line header: control
// characters (C0, C1, DEL) and U+2028/U+2029 become spaces, bidi controls
// are dropped and ']' becomes ')', so a field cannot end the header, start a
// new line or reorder the display. Invalid UTF-8 becomes U+FFFD.
func SanitizeLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case IsBidiControl(r):
		case isLineBreakingRune(r):
			b.WriteByte(' ')
		case r == ']':
			b.WriteByte(')')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeNewlines maps every line-break form (\r\n, \r, \v, \f, U+0085,
// U+2028, U+2029) to "\n", other control characters except tab to a space
// and drops bidi controls, so splitting on "\n" quotes every visual line.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case IsBidiControl(r):
		case r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			b.WriteByte('\n')
		case r == '\t':
			b.WriteByte('\t')
		case isLineBreakingRune(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
