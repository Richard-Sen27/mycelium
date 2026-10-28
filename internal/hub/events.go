package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// The UI's live event stream (EventService.WatchEvents, see uiapi.go and
// proto/mycel/v1/ui.proto). Every store write that changes a node, agent,
// channel, message, job, grant or audit row reports a change through
// store.onChange; freezes are reported by changeFreeze and online/offline
// transitions by a poll of the bus. A single loader goroutine turns changes
// into events (loading the current row as the body), numbers them and keeps
// them in an in-memory ring bounded by count, age and bytes; streams read the
// ring after their cursor and filter each event by the caller's visibility,
// with a per-stream cache so that steady-state filtering needs no queries.

const (
	kindNode    = mycelv1.EventKind_EVENT_KIND_NODE
	kindAgent   = mycelv1.EventKind_EVENT_KIND_AGENT
	kindChannel = mycelv1.EventKind_EVENT_KIND_CHANNEL
	kindMessage = mycelv1.EventKind_EVENT_KIND_MESSAGE
	kindJob     = mycelv1.EventKind_EVENT_KIND_JOB
	kindGrant   = mycelv1.EventKind_EVENT_KIND_GRANT
	kindFreeze  = mycelv1.EventKind_EVENT_KIND_FREEZE
	kindAudit   = mycelv1.EventKind_EVENT_KIND_AUDIT

	opCreated = mycelv1.EventOp_EVENT_OP_CREATED
	opUpdated = mycelv1.EventOp_EVENT_OP_UPDATED
	opDeleted = mycelv1.EventOp_EVENT_OP_DELETED
)

const (
	eventRingMax            = 10000
	eventRingAge            = time.Hour
	eventQueueMax           = 65536 // changes waiting for the loader; beyond, the ring resets
	maxEventStreamsPerUser  = 4
	maxEventStreamsPerScope = 4 // node read scopes, not counted against the owner's own streams
	maxEventStreamsTotal    = 512
	maxEventStream          = 30 * time.Minute
	changeOverhead          = 256 // bytes charged per change or event besides its body
)

// Byte budgets of the ring and of the changes waiting for the loader (tests
// shrink them). Past the ring budget the oldest events are dropped (their
// cursors become "too old"); past the queue budget the queue is dropped and
// the ring resets.
var (
	eventRingBytes  = 64 << 20
	eventQueueBytes = 64 << 20
)

// Tunables (tests shorten them).
var (
	eventHeartbeat  = 15 * time.Second
	eventOnlinePoll = 2 * time.Second
)

// testHookEventsStarted, when set by a test, runs in WatchEvents between
// fixing the stream's cursor and sending the opening heartbeat.
var testHookEventsStarted func()

// change is one row change reported by the store.
type change struct {
	kind mycelv1.EventKind
	op   mycelv1.EventOp
	id   string
	// body is an optional ready event body (a proto message), an audit
	// entry with its owner (auditBody) or the last known row of a deleted
	// object (*nodeRow, *jobRow, *mycelv1.Grant).
	body any
}

// auditBody is an audit entry and the user who owns its actor ("" =
// admins only).
type auditBody struct {
	entry *mycelv1.AuditEntry
	owner string
}

func (c change) size() int {
	switch b := c.body.(type) {
	case proto.Message:
		return changeOverhead + proto.Size(b)
	case auditBody:
		return changeOverhead + proto.Size(b.entry)
	}
	return changeOverhead
}

// ringEntry is one numbered event with what the filters need beside it.
type ringEntry struct {
	ev    *mycelv1.Event
	owner string // audit events: the owner of the actor
	bytes int
}

// eventHub is the ring and the stream registry.
type eventHub struct {
	mu           sync.Mutex
	ring         []ringEntry // oldest first, consecutive seqs
	ringBytes    int
	seq          uint64 // last assigned
	floor        uint64 // smallest cursor that can still be served
	pending      []change
	pendingBytes int
	overflow     bool
	kick         chan struct{}
	waiters      map[chan struct{}]struct{}
	users        map[string]int
	scopes       map[string]int
	streams      int
	now          func() time.Time
	done         chan struct{}
	searches     *rateLimiter // Search calls per user
}

func newEventHub() *eventHub {
	// Sequences start at the clock so that a cursor from before a hub
	// restart is always "too old" instead of silently skipping events.
	base := uint64(time.Now().UnixMicro()) //nolint:gosec // positive
	return &eventHub{
		seq: base, floor: base, kick: make(chan struct{}, 1), waiters: map[chan struct{}]struct{}{},
		users: map[string]int{}, scopes: map[string]int{}, now: time.Now, done: make(chan struct{}),
		searches: newRateLimiter(searchesPerMinute, searchBurst),
	}
}

// emit queues a change; it never blocks (store.onChange).
func (h *eventHub) emit(c change) {
	n := c.size()
	h.mu.Lock()
	if len(h.pending) >= eventQueueMax || h.pendingBytes+n > eventQueueBytes {
		h.overflow = true
		h.pending, h.pendingBytes = h.pending[:0], 0
	} else {
		h.pending = append(h.pending, c)
		h.pendingBytes += n
	}
	h.mu.Unlock()
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *eventHub) take() ([]change, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, o := h.pending, h.overflow
	h.pending, h.pendingBytes, h.overflow = nil, 0, false
	return p, o
}

// append numbers the entries, stores them and wakes the streams.
func (h *eventHub) append(ents []ringEntry, reset bool) {
	h.mu.Lock()
	now := h.now()
	if reset {
		// Changes were lost: every open cursor is too old now.
		h.seq++
		h.ring, h.ringBytes = nil, 0
		h.floor = h.seq
	}
	for _, e := range ents {
		h.seq++
		e.ev.Seq = h.seq
		e.ev.At = formatTime(now)
		e.bytes = changeOverhead + proto.Size(e.ev)
		h.ring = append(h.ring, e)
		h.ringBytes += e.bytes
	}
	cut, bytes := 0, h.ringBytes
	for cut < len(h.ring) {
		e := h.ring[cut]
		t, err := parseTime(e.ev.GetAt())
		old := err != nil || now.Sub(t) > eventRingAge
		if len(h.ring)-cut <= eventRingMax && bytes <= eventRingBytes && !old {
			break
		}
		bytes -= e.bytes
		cut++
	}
	if cut > 0 {
		h.floor = h.ring[cut-1].ev.GetSeq()
		h.ring = slices.Clone(h.ring[cut:])
		h.ringBytes = bytes
	}
	for w := range h.waiters {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

var errEventsTooOld = connect.NewError(connect.CodeFailedPrecondition, errors.New("too old"))

// start resolves a client's after_seq into a cursor.
func (h *eventHub) start(after uint64) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if after == 0 {
		return h.seq, nil
	}
	if after > h.seq || after < h.floor {
		return 0, errEventsTooOld
	}
	return after, nil
}

// after returns the entries after cursor (at most limit).
func (h *eventHub) after(cursor uint64, limit int) ([]ringEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cursor < h.floor || cursor > h.seq {
		return nil, errEventsTooOld
	}
	if len(h.ring) == 0 || cursor >= h.seq {
		return nil, nil
	}
	i := int(cursor + 1 - h.ring[0].ev.GetSeq()) //nolint:gosec // cursor >= floor = ring[0].seq-1
	j := min(len(h.ring), i+limit)
	return slices.Clone(h.ring[i:j]), nil
}

func (h *eventHub) subscribe() (<-chan struct{}, func()) {
	w := make(chan struct{}, 1)
	h.mu.Lock()
	h.waiters[w] = struct{}{}
	h.mu.Unlock()
	return w, func() {
		h.mu.Lock()
		delete(h.waiters, w)
		h.mu.Unlock()
	}
}

// acquire reserves a stream: per node scope when scope is set (those do not
// count against the owner's own streams), else per user.
func (h *eventHub) acquire(user, scope string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams >= maxEventStreamsTotal {
		return false
	}
	if scope != "" {
		if h.scopes[scope] >= maxEventStreamsPerScope {
			return false
		}
		h.scopes[scope]++
	} else {
		if h.users[user] >= maxEventStreamsPerUser {
			return false
		}
		h.users[user]++
	}
	h.streams++
	return true
}

func (h *eventHub) release(user, scope string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams--
	m, k := h.users, user
	if scope != "" {
		m, k = h.scopes, scope
	}
	if m[k]--; m[k] <= 0 {
		delete(m, k)
	}
}

// runEvents is the loader: it turns changes into events until stop closes.
func (s *server) runEvents(stop <-chan struct{}) {
	h := s.events
	defer close(h.done)
	poll := time.NewTicker(eventOnlinePoll)
	defer poll.Stop()
	online := map[string]bool{}
	for {
		select {
		case <-stop:
			return
		case <-h.kick:
		case <-poll.C:
			now := map[string]bool{}
			if s.bus != nil {
				for _, n := range s.bus.ConnectedNodes() {
					now[n] = true
				}
			}
			for n := range now {
				if !online[n] {
					h.emit(change{kind: kindNode, op: opUpdated, id: n})
				}
			}
			for n := range online {
				if !now[n] {
					h.emit(change{kind: kindNode, op: opUpdated, id: n})
				}
			}
			online = now
			continue
		}
		changes, reset := h.take()
		if len(changes) == 0 && !reset {
			continue
		}
		ctx := context.Background()
		ents := make([]ringEntry, 0, len(changes))
		for _, c := range changes {
			if ev := s.eventFor(ctx, c); ev != nil {
				e := ringEntry{ev: ev}
				if ab, ok := c.body.(auditBody); ok {
					e.owner = ab.owner
				}
				ents = append(ents, e)
			}
		}
		if reset {
			s.log.Warn("event queue overflowed; open event streams must reload")
		}
		h.append(ents, reset)
		for _, e := range ents {
			s.pushOnEvent(e.ev)
		}
	}
}

// startEvents starts the loader; the returned func stops it and waits.
func (s *server) startEvents() func() {
	stop := make(chan struct{})
	go s.runEvents(stop)
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-s.events.done
		})
	}
}

// eventFor builds the event of a change, loading the current row. Rows that
// are gone become id-only events (deletes keep their last known row).
func (s *server) eventFor(ctx context.Context, c change) *mycelv1.Event {
	ev := &mycelv1.Event{Kind: c.kind, Op: c.op, Id: c.id}
	switch c.kind {
	case kindNode:
		if n, ok := c.body.(*nodeRow); ok {
			ev.Body = &mycelv1.Event_Node{Node: s.nodeToProto(n, false)}
			break
		}
		if n, err := s.st.getNode(ctx, c.id); err == nil {
			online := false
			if s.bus != nil {
				online = s.onlineSet()[n.Name]
			}
			ev.Body = &mycelv1.Event_Node{Node: s.nodeToProto(n, online)}
		} else if errors.Is(err, errNotFound) {
			ev.Op = opDeleted
		}
	case kindAgent:
		if a, err := s.st.getAgent(ctx, c.id); err == nil {
			ev.Body = &mycelv1.Event_Agent{Agent: agentToProto(a)}
		}
	case kindChannel:
		if ch, err := s.st.getChannel(ctx, c.id); err == nil {
			if pc, err := s.channelToProto(ctx, ch); err == nil {
				ev.Body = &mycelv1.Event_Channel{Channel: pc}
			}
		}
	case kindMessage:
		m, ok := c.body.(*mycelv1.Message)
		if !ok {
			return nil
		}
		ev.Body = &mycelv1.Event_Message{Message: m}
	case kindJob:
		if j, ok := c.body.(*jobRow); ok && c.op == opDeleted {
			ev.Body = &mycelv1.Event_Job{Job: &mycelv1.Job{Id: j.ID, OwnerId: j.OwnerID}}
			break
		}
		if j, err := s.st.getJob(ctx, c.id); err == nil {
			if pj, err := s.jobProto(ctx, j); err == nil {
				ev.Body = &mycelv1.Event_Job{Job: pj}
			}
		}
	case kindGrant:
		if g, ok := c.body.(*mycelv1.Grant); ok && c.op == opDeleted {
			ev.Body = &mycelv1.Event_Grant{Grant: g}
			break
		}
		if g, err := s.st.getGrant(ctx, c.id); err == nil {
			_ = s.withUsage(ctx, g)
			ev.Body = &mycelv1.Event_Grant{Grant: g}
		}
	case kindFreeze:
		f, ok := c.body.(*mycelv1.FreezeStatusResponse)
		if !ok {
			return nil
		}
		ev.Body = &mycelv1.Event_Freeze{Freeze: f}
	case kindAudit:
		a, ok := c.body.(auditBody)
		if !ok {
			return nil
		}
		ev.Body = &mycelv1.Event_Audit{Audit: a.entry}
	default:
		return nil
	}
	return ev
}

// freezeStatusLocked builds the freeze status; f.mu must be held.
func (s *server) freezeStatusLocked() *mycelv1.FreezeStatusResponse {
	out := &mycelv1.FreezeStatusResponse{}
	if s.freeze.all != nil {
		out.AllFrozen, out.AllReason, out.AllSince, out.AllBy = true, s.freeze.all.Reason, s.freeze.all.Since, s.freeze.all.By
	}
	for name := range s.freeze.channels {
		out.FrozenChannels = append(out.FrozenChannels, name)
	}
	slices.Sort(out.FrozenChannels)
	return out
}

// emitFreeze reports a freeze change: before is the status before it, the
// cache is locked and already updated.
func (s *server) emitFreeze(before *mycelv1.FreezeStatusResponse) {
	after := s.freezeStatusLocked()
	id := freezeScopeAll
	if before.GetAllFrozen() == after.GetAllFrozen() {
		for _, n := range symmetricDiff(before.GetFrozenChannels(), after.GetFrozenChannels()) {
			id = channelScope(n)
		}
	}
	s.events.emit(change{kind: kindFreeze, op: opUpdated, id: id, body: after})
}

func symmetricDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) {
			out = append(out, x)
		}
	}
	return out
}

// ---- visibility ----

// viewer is whoever reads events and hits: a user (for a node read scope,
// the node owner as a member). Its caches live for one stream (or one
// search); invalidate drops what an event may have changed and reset
// drops everything (on every heartbeat).
type viewer struct {
	u        *userRow
	channels map[string]bool   // "<get|read> <channel>" -> allowed
	nodes    map[string]string // node -> owner ("" when gone)
	queries  int               // store queries made (tests)
}

func newViewer(u *userRow) *viewer {
	v := &viewer{u: u}
	v.reset()
	return v
}

func (v *viewer) reset() {
	v.channels = map[string]bool{}
	v.nodes = map[string]string{}
}

func (v *viewer) admin() bool { return v.u.Role == roleAdmin }

// invalidate drops cached answers an event may change: channel access on a
// channel event, the owner on a node event.
func (v *viewer) invalidate(ev *mycelv1.Event) {
	switch ev.GetKind() {
	case kindChannel:
		for k := range v.channels {
			if strings.HasSuffix(k, " "+ev.GetId()) {
				delete(v.channels, k)
			}
		}
	case kindNode:
		delete(v.nodes, ev.GetId())
	}
}

// visible reports whether v may see ev (owner: the owner of an audit
// entry's actor): nodes, agents and freezes every user (as ListNodes,
// ListAgents and FreezeStatus show them); jobs their owner; grants the
// subject's owner and the node owner; channels as GetChannel; messages as
// Watch, except that a direct channel's messages go only to its members
// (an admin reads a direct channel through ListMessages or Watch, which
// audit it); audit entries the actor's owner. Admins see everything else.
func (s *server) visible(ctx context.Context, v *viewer, ev *mycelv1.Event, owner string) bool {
	switch ev.GetKind() {
	case kindNode, kindFreeze, kindAgent:
		return true
	case kindMessage:
		m := ev.GetMessage()
		return m != nil && s.canSeeChannel(ctx, v, m.GetChannel(), policy.ActionChannelRead)
	}
	if v.admin() {
		return true
	}
	switch ev.GetKind() {
	case kindJob:
		j := ev.GetJob()
		return j != nil && j.GetOwnerId() == v.u.ID
	case kindGrant:
		g := ev.GetGrant()
		return g != nil && (g.GetOwnerId() == v.u.ID || s.nodeOwnerCached(ctx, v, g.GetNode()) == v.u.ID)
	case kindChannel:
		c := ev.GetChannel()
		return c != nil && s.canSeeChannel(ctx, v, c.GetName(), policy.ActionChannelGet)
	case kindAudit:
		return owner != "" && owner == v.u.ID
	}
	return false
}

func (s *server) nodeOwnerCached(ctx context.Context, v *viewer, node string) string {
	if owner, ok := v.nodes[node]; ok {
		return owner
	}
	v.queries++
	owner := ""
	if n, err := s.st.getNode(ctx, node); err == nil {
		owner = n.OwnerID
	}
	v.nodes[node] = owner
	return owner
}

// canSeeChannel checks action on channel name for the viewer (cached). For
// reads of a direct channel only explicit members qualify, admins included.
func (s *server) canSeeChannel(ctx context.Context, v *viewer, name string, action policy.Action) bool {
	key := string(action) + " " + name
	if ok, seen := v.channels[key]; seen {
		return ok
	}
	v.queries++
	ok := false
	if ch, err := s.st.getChannel(ctx, name); err == nil {
		member, d, err := s.checkChannel(ctx, userActor(v.u), action, ch)
		ok = err == nil && d.Allow
		if isDirect(ch) && action == policy.ActionChannelRead {
			ok = ok && member
		}
	}
	v.channels[key] = ok
	return ok
}

// ---- EventService ----

type eventService struct{ s *server }

var _ mycelv1connect.EventServiceHandler = (*eventService)(nil)

// streamDeadlinesFor hands the handler of the streaming procedure its
// response controller so it can lift the server timeouts once authorised
// (see streamDeadlines).
func streamDeadlinesFor(procedure string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == procedure {
			r = r.WithContext(context.WithValue(r.Context(), ctxStream, http.NewResponseController(w)))
		}
		h.ServeHTTP(w, r)
	})
}

func validKinds(kinds []mycelv1.EventKind) (map[mycelv1.EventKind]bool, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	if len(kinds) > 16 {
		return nil, invalidArg("too many kinds")
	}
	out := map[mycelv1.EventKind]bool{}
	for _, k := range kinds {
		if k <= mycelv1.EventKind_EVENT_KIND_UNSPECIFIED || k > kindAudit {
			return nil, invalidArg("unknown event kind %d", k)
		}
		out[k] = true
	}
	return out, nil
}

// streamValid re-checks a stream's caller (on every heartbeat): the cookie
// session still exists, the user still exists with the same role, and a
// node read scope's certificate is not revoked and its node still belongs
// to the same owner.
func (s *server) streamValid(ctx context.Context, u *userRow) error {
	gone := func(why string) error { return connect.NewError(connect.CodeUnauthenticated, errors.New(why)) }
	if sid := sessionOf(ctx); sid != "" {
		if sess, ok := s.ui.session(sid); !ok || sess.userID != u.ID {
			return gone("session ended")
		}
	}
	if scope := readScopeOf(ctx); scope != "" {
		np := nodeFrom(ctx)
		if np == nil || s.st.IsRevoked(identity.SerialHex(np.Cert), identity.NodeSPIFFEID(s.td, np.Name)) {
			return gone("node certificate revoked")
		}
		if n, err := s.st.getNode(ctx, scope); err != nil || n.OwnerID != u.ID {
			return gone("node removed or re-owned")
		}
	}
	cur, err := s.st.getUser(ctx, u.ID)
	if errors.Is(err, errNotFound) {
		return gone("user removed")
	}
	if err == nil && readScopeOf(ctx) == "" && cur.Role != u.Role {
		return gone("role changed; reconnect")
	}
	return nil
}

// WatchEvents streams every change the caller may see, after after_seq
// (replayed from the ring) or from now, until the client goes away, the hub
// stops, the caller's session or rights end, or maxEventStream passes (the
// client resumes with after_seq).
//
// Opening heartbeat (hydration): the first message is an event of kind
// UNSPECIFIED with no body whose seq is the stream's resume point, fixed
// when the stream started (after_seq, or the latest sequence for "from
// now"). Every later event has a larger seq, so a client that opens the
// stream, then loads its lists, and drops events with seq <= that value
// misses nothing; it resumes with after_seq = the last seq it saw. After
// that a heartbeat (no event) follows every 15 s.
func (e *eventService) WatchEvents(ctx context.Context, req *connect.Request[mycelv1.WatchEventsRequest], stream *connect.ServerStream[mycelv1.WatchEventsResponse]) error {
	s := e.s
	u, err := requireUser(ctx)
	if err != nil {
		return err
	}
	kinds, err := validKinds(req.Msg.GetKinds())
	if err != nil {
		return err
	}
	scope := readScopeOf(ctx)
	if !s.events.acquire(u.ID, scope) {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
			"too many event streams (at most %d per user, %d per node scope)", maxEventStreamsPerUser, maxEventStreamsPerScope))
	}
	defer s.events.release(u.ID, scope)
	wake, cancel := s.events.subscribe()
	defer cancel()
	cursor, err := s.events.start(req.Msg.GetAfterSeq())
	if err != nil {
		return err
	}
	liftStreamDeadlines(ctx)
	if testHookEventsStarted != nil {
		testHookEventsStarted()
	}
	if err := stream.Send(&mycelv1.WatchEventsResponse{Event: &mycelv1.Event{Seq: cursor}}); err != nil {
		return nil
	}
	hb := time.NewTicker(eventHeartbeat)
	defer hb.Stop()
	limit := time.NewTimer(maxEventStream)
	defer limit.Stop()
	v := newViewer(u)
	for {
		for {
			batch, err := s.events.after(cursor, scanBatch)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for _, ent := range batch {
				ev := ent.ev
				cursor = ev.GetSeq()
				v.invalidate(ev)
				if kinds != nil && !kinds[ev.GetKind()] {
					continue
				}
				if !s.visible(ctx, v, ev, ent.owner) {
					continue
				}
				if err := stream.Send(&mycelv1.WatchEventsResponse{Event: ev}); err != nil {
					return nil
				}
			}
		}
		select {
		case <-wake:
		case <-hb.C:
			if err := s.streamValid(ctx, u); err != nil {
				return err
			}
			v.reset()
			if err := stream.Send(&mycelv1.WatchEventsResponse{}); err != nil {
				return nil
			}
		case <-ctx.Done():
			return nil
		case <-s.ch.stop:
			return nil
		case <-limit.C:
			return nil
		}
	}
}
