package hub

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const (
	maxSearchQuery     = 200
	defaultSearchLimit = 50
	maxSearchLimit     = 200
	maxSnippet         = 200
	searchScanRows     = 1000 // rows of one kind looked at per search
	searchesPerMinute  = 600  // 10/s per user
	searchBurst        = 10
)

// Message search walks the body index downwards in seq windows: each LIKE
// query examines at most searchWindow rows, and a search stops after
// searchWindows windows (the newest 100 000 messages). Tests shrink them.
var (
	searchWindow  int64 = 5000
	searchWindows       = 20
)

// Search finds nodes, agents, channels, jobs, grants and messages whose
// names, titles or bodies contain the query (case-insensitive), filtered by
// the caller's visibility (the same rules as the event stream).
func (e *eventService) Search(ctx context.Context, req *connect.Request[mycelv1.SearchRequest]) (*connect.Response[mycelv1.SearchResponse], error) {
	s := e.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if !s.events.searches.allow(u.ID) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many searches; slow down"))
	}
	q := strings.TrimSpace(req.Msg.GetQuery())
	if q == "" || len(q) > maxSearchQuery || !utf8.ValidString(q) {
		return nil, invalidArg("query must be 1-%d bytes of text", maxSearchQuery)
	}
	kinds, err := validKinds(req.Msg.GetKinds())
	if err != nil {
		return nil, err
	}
	if kinds[kindAudit] {
		return nil, invalidArg("audit entries are searched with ListAudit")
	}
	if kinds[kindFreeze] {
		return nil, invalidArg("freezes are not searchable; use FreezeStatus")
	}
	want := func(k mycelv1.EventKind) bool { return kinds == nil || kinds[k] }
	limit := clampLimit(req.Msg.GetLimit(), defaultSearchLimit, maxSearchLimit)
	sr := &searcher{s: s, v: newViewer(u), q: q, lq: strings.ToLower(q), limit: limit}
	steps := []struct {
		kind mycelv1.EventKind
		fn   func(context.Context) error
	}{
		{kindNode, sr.nodes}, {kindAgent, sr.agents}, {kindChannel, sr.channels},
		{kindJob, sr.jobs}, {kindGrant, sr.grants}, {kindMessage, sr.messages},
	}
	for _, st := range steps {
		if !want(st.kind) || sr.full() {
			continue
		}
		if err := st.fn(ctx); err != nil {
			return nil, s.internalError("search", err)
		}
	}
	return connect.NewResponse(&mycelv1.SearchResponse{Hits: sr.hits}), nil
}

type searcher struct {
	s     *server
	v     *viewer
	q, lq string
	limit int
	hits  []*mycelv1.SearchHit
}

func (sr *searcher) full() bool { return len(sr.hits) >= sr.limit }

// add appends a hit when the viewer may see ev.
func (sr *searcher) add(ctx context.Context, ev *mycelv1.Event, hit *mycelv1.SearchHit) {
	if sr.full() || !sr.s.visible(ctx, sr.v, ev, "") {
		return
	}
	hit.Kind = ev.GetKind()
	sr.hits = append(sr.hits, hit)
}

func (sr *searcher) match(fields ...string) (string, bool) {
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), sr.lq) {
			return f, true
		}
	}
	return "", false
}

func (sr *searcher) nodes(ctx context.Context) error {
	names, err := sr.s.st.searchNames(ctx, "nodes", "name", sr.q, searchScanRows)
	if err != nil {
		return err
	}
	for _, n := range names {
		row, err := sr.s.st.getNode(ctx, n)
		if err != nil {
			continue
		}
		node := sr.s.nodeToProto(row, false)
		sr.add(ctx, &mycelv1.Event{Kind: kindNode, Body: &mycelv1.Event_Node{Node: node}}, &mycelv1.SearchHit{
			Id: row.Name, Title: row.Name, Snippet: snippet(row.Name, sr.lq), At: firstOf(row.LastSeenAt, row.CreatedAt),
		})
	}
	return nil
}

func (sr *searcher) agents(ctx context.Context) error {
	ids, err := sr.s.st.searchNames(ctx, "agents", "id", sr.q, searchScanRows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		row, err := sr.s.st.getAgent(ctx, id)
		if err != nil {
			continue
		}
		a := agentToProto(row)
		sr.add(ctx, &mycelv1.Event{Kind: kindAgent, Body: &mycelv1.Event_Agent{Agent: a}}, &mycelv1.SearchHit{
			Id: row.ID, Title: row.ID, Snippet: snippet(row.ID, sr.lq), At: firstOf(row.ExitedAt, row.CreatedAt),
		})
	}
	return nil
}

func (sr *searcher) channels(ctx context.Context) error {
	names, err := sr.s.st.searchNames(ctx, "channels", "name", sr.q, searchScanRows)
	if err != nil {
		return err
	}
	for _, n := range names {
		row, err := sr.s.st.getChannel(ctx, n)
		if err != nil {
			continue
		}
		c := &mycelv1.Channel{Name: row.Name, Type: row.Type, OwnerId: row.OwnerID}
		sr.add(ctx, &mycelv1.Event{Kind: kindChannel, Body: &mycelv1.Event_Channel{Channel: c}}, &mycelv1.SearchHit{
			Id: row.Name, Title: row.Name, Snippet: snippet(row.Name, sr.lq), At: firstOf(row.ArchivedAt, row.CreatedAt),
		})
	}
	return nil
}

func (sr *searcher) jobs(ctx context.Context) error {
	owner := sr.v.u.ID
	if sr.v.admin() {
		owner = ""
	}
	rows, err := sr.s.st.listJobs(ctx, owner, true, searchScanRows)
	if err != nil {
		return err
	}
	for _, j := range rows {
		cmd := strings.Join(j.Spec.GetCommand(), " ")
		text, ok := sr.match(j.Spec.GetName(), cmd, j.ID)
		if !ok {
			continue
		}
		title := j.Spec.GetName()
		if title == "" {
			title = cmd
		}
		job := &mycelv1.Job{Id: j.ID, OwnerId: j.OwnerID}
		sr.add(ctx, &mycelv1.Event{Kind: kindJob, Body: &mycelv1.Event_Job{Job: job}}, &mycelv1.SearchHit{
			Id: j.ID, Title: clip(title, maxSnippet), Snippet: snippet(text, sr.lq), At: firstOf(j.FinishedAt, j.StartedAt, j.CreatedAt),
		})
	}
	return nil
}

func (sr *searcher) grants(ctx context.Context) error {
	f := grantFilter{limit: searchScanRows}
	if !sr.v.admin() {
		f.visibleTo = sr.v.u.ID
	}
	rows, err := sr.s.st.listGrants(ctx, f)
	if err != nil {
		return err
	}
	for _, g := range rows {
		kind := grantKind(g.GetSpec())
		text, ok := sr.match(kind+" "+g.GetSubject(), g.GetId(), g.GetNode())
		if !ok {
			continue
		}
		sr.add(ctx, &mycelv1.Event{Kind: kindGrant, Body: &mycelv1.Event_Grant{Grant: g}}, &mycelv1.SearchHit{
			Id: g.GetId(), Title: kind + " " + g.GetSubject(), Snippet: snippet(text, sr.lq),
			At: firstOf(g.GetActivatedAt(), g.GetCreatedAt()),
		})
	}
	return nil
}

func (sr *searcher) messages(ctx context.Context) error {
	hi, err := sr.s.st.maxMessageSeq(ctx)
	if err != nil {
		return err
	}
	for w := 0; w < searchWindows && hi > 0 && !sr.full(); w++ {
		batch, err := sr.s.st.searchMessages(ctx, sr.q, hi, searchWindow)
		if err != nil {
			return err
		}
		hi -= searchWindow
		for _, m := range batch {
			// SQLite LIKE folds ASCII only; confirm the match here.
			if _, ok := sr.match(messageText(m)); !ok {
				continue
			}
			sr.add(ctx, &mycelv1.Event{Kind: kindMessage, Body: &mycelv1.Event_Message{Message: m}}, &mycelv1.SearchHit{
				Id: m.GetId(), Title: m.GetChannel(), Snippet: snippet(messageText(m), sr.lq), Channel: m.GetChannel(), At: m.GetTs(),
			})
		}
	}
	return nil
}

func firstOf(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// snippet is the text around the first match of lq, at most maxSnippet
// bytes of valid UTF-8.
func snippet(text, lq string) string {
	if len(text) <= maxSnippet {
		return text
	}
	i := strings.Index(strings.ToLower(text), lq)
	start := 0
	if i > 60 && i < len(text) {
		start = i - 60
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	return clip(text[start:], maxSnippet)
}

// clip cuts s to at most n bytes at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
