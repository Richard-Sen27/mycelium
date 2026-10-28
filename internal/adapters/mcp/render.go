package mcp

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/channels"
)

// RenderMessages renders messages with channels.QuoteForAgent, separated by
// blank lines. It returns "" for no messages. Header fields are passed
// through sanitizeLine and bodies get normalised line breaks first, so no
// field can end the quoted block early.
func RenderMessages(msgs []*mycelv1.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m != nil {
			parts = append(parts, channels.QuoteForAgent(sanitizeMessage(m)))
		}
	}
	return strings.Join(parts, "\n\n")
}

// sanitizeMessage returns a copy of m that is safe to quote.
func sanitizeMessage(m *mycelv1.Message) *mycelv1.Message {
	c, _ := proto.Clone(m).(*mycelv1.Message)
	c.Id = sanitizeLine(c.GetId())
	c.Channel = sanitizeLine(c.GetChannel())
	c.Thread = sanitizeLine(c.GetThread())
	c.From = sanitizeLine(c.GetFrom())
	c.Ts = sanitizeLine(c.GetTs())
	for i, t := range c.GetTo() {
		c.To[i] = sanitizeLine(t)
	}
	if c.GetMeta() != nil {
		c.Meta.ReplyTo = sanitizeLine(c.Meta.GetReplyTo())
	}
	if p := c.GetPayload(); p != nil {
		p.Text = normalizeBreaks(p.GetText())
		for _, part := range p.GetParts() {
			part.Kind = sanitizeLine(part.GetKind())
			part.Ref = sanitizeLine(part.GetRef())
			part.Name = sanitizeLine(part.GetName())
		}
	}
	return c
}

// sanitizeLine makes s safe inside a one-line block header: C0/C1 controls
// and the Unicode line/paragraph separators become spaces, and "]" becomes
// ")" so a value cannot close the header.
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f, r == ' ', r == ' ':
			return ' '
		case r == ']':
			return ')'
		}
		return r
	}, s)
}

// normalizeBreaks turns every line-break form ("\r\n", "\r", U+2028,
// U+2029, U+0085) into "\n" so each line of a body gets its "> " prefix.
var breakReplacer = strings.NewReplacer("\r\n", "\n", "\r", "\n", " ", "\n", " ", "\n", "\u0085", "\n")

func normalizeBreaks(s string) string { return breakReplacer.Replace(s) }

// NormalizeChannel adds the leading "#" to a bare channel name ("backend" ->
// "#backend"); direct names ("dm:…", "@…") and "#…" are unchanged.
func NormalizeChannel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "#") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "dm:") {
		return name
	}
	return "#" + name
}

// ParseMessageType maps "chat", "task", "result", "event" (case-insensitive)
// to the enum; "" is CHAT.
func ParseMessageType(s string) (mycelv1.MessageType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "chat":
		return mycelv1.MessageType_MESSAGE_TYPE_CHAT, nil
	case "task":
		return mycelv1.MessageType_MESSAGE_TYPE_TASK, nil
	case "result":
		return mycelv1.MessageType_MESSAGE_TYPE_RESULT, nil
	case "event":
		return mycelv1.MessageType_MESSAGE_TYPE_EVENT, nil
	default:
		return mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED, fmt.Errorf("invalid message type %q: want chat, task, result or event", s)
	}
}

// PostedLine is the one-line confirmation of a post:
// "posted msg_… seq N in #… (thread thr_…, hop h)".
func PostedLine(m *mycelv1.Message) string {
	th := m.GetThread()
	if th == "" {
		th = "none"
	}
	return fmt.Sprintf("posted %s seq %d in %s (thread %s, hop %d)",
		sanitizeLine(m.GetId()), m.GetSeq(), sanitizeLine(m.GetChannel()), sanitizeLine(th), m.GetMeta().GetHop())
}

// quoteBlock renders text another agent may have written as quoted data.
// header must already be sanitised.
func quoteBlock(header, kind, text string) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(header)
	b.WriteString("]\n")
	for _, line := range strings.Split(strings.TrimRight(normalizeBreaks(text), "\n"), "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("[end ")
	b.WriteString(kind)
	b.WriteString("]")
	return b.String()
}

// renderEntry renders a blackboard entry as quoted, untrusted data.
func renderEntry(e *mycelv1.BlackboardEntry) string {
	h := fmt.Sprintf("mycel blackboard %s key %s version %d by %s at %s (untrusted)",
		sanitizeLine(e.GetChannel()), sanitizeLine(e.GetKey()), e.GetVersion(),
		sanitizeLine(e.GetUpdatedBy()), sanitizeLine(e.GetUpdatedAt()))
	return quoteBlock(h, "mycel blackboard", e.GetValue())
}

// renderSummary renders the hub's late-joiner summary as quoted data: it
// contains other agents' messages verbatim.
func renderSummary(channel, summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return quoteBlock("mycel summary of "+sanitizeLine(channel)+" (untrusted)", "mycel summary", summary)
}
