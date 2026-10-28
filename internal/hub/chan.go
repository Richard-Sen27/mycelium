package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/channels"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// Message origins (Provenance.origin).
const (
	originHuman = "human"
	originAgent = "agent"
	originHub   = "hub"
)

// Member roles.
const (
	memberOwner    = "owner"
	memberHuman    = "human"
	memberAgent    = "agent"
	memberObserver = "observer"
)

// Channel limits beyond the domain constants.
const (
	maxTags          = 16
	maxTagLen        = 32
	maxParts         = 16
	maxPartKind      = 16
	maxPartRef       = 256
	maxPartName      = 256
	maxBBKey         = 256
	maxPolicyHops    = 100
	maxPolicyRate    = 6000
	maxProfiles      = 32
	maxWatchChannels = 64
	maxWatch         = 30 * time.Minute
	scanBatch        = 500
	defaultInbox     = 20
	maxInbox         = 200
	maxInboxWait     = 60 * time.Second
	defaultList      = 50
	maxList          = 500

	// Concurrency caps for agent RPCs. Waiting long polls are counted apart
	// from other RPCs so they cannot starve them.
	maxAgentRPCs     = 1024 // all agent RPCs being decoded or served, hub-wide
	maxLongPolls     = 4096 // waiting long polls, hub-wide
	maxNodeRPCs      = 64   // agent RPCs (other than long polls) per node
	maxNodePolls     = 16   // waiting long polls per node
	maxPullsPerAgent = 4    // waiting long polls per agent

	// Rate caps.
	agentGlobalRatePerMinute = 120 // one agent's messages across all channels
	hubEventsPerMinute       = 10  // hop-limit events per channel
	nodeAlertsPerMinute      = 10  // #alerts posts about one node
	deniedAuditEvery         = 10 * time.Second

	// Watch streams.
	maxWatchesPerUser = 8
	maxWatchesTotal   = 256

	publishTimeout = 2 * time.Second
)

// Stream sizes, derived from the bus JetStream store limit; the oldest
// entries go first (SQLite keeps everything).
const (
	streamChannelsMaxBytes = bus.DefaultJetStreamMaxStore / 2
	streamAuditMaxBytes    = bus.DefaultJetStreamMaxStore / 4
	streamJobLogsMaxBytes  = bus.DefaultJetStreamMaxStore / 4
)

// Rate limiter keys that cannot collide with channel names.
const (
	rateKeyAllChannels = "*all"
	rateKeyHub         = "hub"
)

// chanState is the in-memory channel machinery of a running hub.
type chanState struct {
	js      jetstream.JetStream // nil until the streams exist
	notify  *notifier
	limiter *channels.RateLimiter

	hopMu sync.Mutex // serialises the hop-event check and post

	stop     chan struct{} // closed when the hub stops
	stopOnce sync.Once
	workMu   sync.Mutex
	closing  bool
	work     sync.WaitGroup
	slots    chan struct{} // maxAgentRPCs
	polls    chan struct{} // maxLongPolls

	mu          sync.Mutex
	nodeRPCs    map[string]int
	nodePolls   map[string]int
	agentPolls  map[string]int
	userWatches map[string]int
	watches     int
	deniedAt    map[string]time.Time // actor -> last audited denial
}

func newChanState() *chanState {
	return &chanState{
		notify: newNotifier(), limiter: channels.NewRateLimiter(nil),
		stop: make(chan struct{}), slots: make(chan struct{}, maxAgentRPCs), polls: make(chan struct{}, maxLongPolls),
		nodeRPCs: map[string]int{}, nodePolls: map[string]int{}, agentPolls: map[string]int{},
		userWatches: map[string]int{}, deniedAt: map[string]time.Time{},
	}
}

// shutdown wakes every waiter and refuses new background work.
func (c *chanState) shutdown() {
	c.stopOnce.Do(func() {
		c.workMu.Lock()
		c.closing = true
		c.workMu.Unlock()
		close(c.stop)
	})
}

// goWork runs fn in the background in one of the maxAgentRPCs slots unless
// the hub is stopping or the pool is full. fn may call release to give its
// slot back early (a long poll moves to the long-poll pool).
func (c *chanState) goWork(fn func(release func())) bool {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	if c.closing {
		return false
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return false
	}
	c.work.Add(1)
	go func() {
		var once sync.Once
		release := func() { once.Do(func() { <-c.slots }) }
		defer func() { release(); c.work.Done() }()
		fn(release)
	}()
	return true
}

// waitWork waits for background work to finish, up to d.
func (c *chanState) waitWork(d time.Duration) {
	done := make(chan struct{})
	go func() { c.work.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// acquire increments m[key] unless it is at limit.
func (c *chanState) acquire(m map[string]int, key string, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m[key] >= limit {
		return false
	}
	m[key]++
	return true
}

func (c *chanState) release(m map[string]int, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m[key]--; m[key] <= 0 {
		delete(m, key)
	}
}

func (c *chanState) count(m map[string]int, key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return m[key]
}

// acquireWatch reserves a Watch stream for user.
func (c *chanState) acquireWatch(user string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watches >= maxWatchesTotal || c.userWatches[user] >= maxWatchesPerUser {
		return false
	}
	c.watches++
	c.userWatches[user]++
	return true
}

func (c *chanState) releaseWatch(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watches--
	if c.userWatches[user]--; c.userWatches[user] <= 0 {
		delete(c.userWatches, user)
	}
}

// sampleDenied reports whether a denial by actor should be audited now (at
// most one per deniedAuditEvery).
func (c *chanState) sampleDenied(actor string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.deniedAt[actor]; ok && now.Sub(last) < deniedAuditEvery {
		return false
	}
	if len(c.deniedAt) > 10000 {
		for k, t := range c.deniedAt {
			if now.Sub(t) >= deniedAuditEvery {
				delete(c.deniedAt, k)
			}
		}
	}
	c.deniedAt[actor] = now
	return true
}

// auditDenied audits a refused request, sampled per actor so that a client
// retrying a refused call cannot flood the audit log; the rest go to the
// debug log.
func (s *server) auditDenied(ctx context.Context, actor, action, resource string, details map[string]any) {
	if s.ch.sampleDenied(actor, time.Now()) {
		s.audit(ctx, actor, action, resource, outcomeDenied, details)
		return
	}
	s.log.Debug("denied (audit sampled)", "actor", actor, "action", action, "resource", resource, "details", details)
}

// ---- notifier ----

// watchAll is the notifier key signalled for every message (Watch streams).
const watchAll = "*"

// notifier wakes waiters by key (an agent id, or watchAll). A waiter's
// channel has room for one signal, so a signal between registering and
// waiting is never lost.
type notifier struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newNotifier() *notifier { return &notifier{subs: map[string]map[chan struct{}]struct{}{}} }

func (n *notifier) subscribe(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subs[key] == nil {
		n.subs[key] = map[chan struct{}]struct{}{}
	}
	n.subs[key][ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.subs[key], ch)
		if len(n.subs[key]) == 0 {
			delete(n.subs, key)
		}
		n.mu.Unlock()
	}
}

func (n *notifier) signal(keys ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, k := range keys {
		for ch := range n.subs[k] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// ---- JetStream ----

// setupStreams creates (or updates) the CH, AUDIT and JOBLOG streams.
//
// CH holds one protobuf Message per accepted message on ch.<name>.msg. The
// stream sequence is not the hub sequence (publishes are best effort and
// may be retried or lost); consumers order and de-duplicate by
// Message.seq and Message.id.
func (s *server) setupStreams(ctx context.Context) error {
	js, err := jetstream.New(s.nc)
	if err != nil {
		return fmt.Errorf("hub: jetstream: %w", err)
	}
	for _, cfg := range []jetstream.StreamConfig{
		{
			Name: bus.StreamChannels, Subjects: []string{bus.StreamChannelsSubject}, Storage: jetstream.FileStorage,
			MaxBytes: streamChannelsMaxBytes, Discard: jetstream.DiscardOld, Duplicates: 2 * time.Minute,
		},
		{
			Name: bus.StreamAudit, Subjects: []string{bus.StreamAuditSubject}, Storage: jetstream.FileStorage,
			MaxBytes: streamAuditMaxBytes, Discard: jetstream.DiscardOld,
		},
		{
			Name: bus.StreamJobLogs, Subjects: []string{bus.StreamJobLogsSubject}, Storage: jetstream.FileStorage,
			MaxBytes: streamJobLogsMaxBytes, Discard: jetstream.DiscardOld,
		},
	} {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("hub: stream %s: %w", cfg.Name, err)
		}
	}
	s.ch.js = js
	return nil
}

func (s *server) publishMessage(msg *mycelv1.Message) {
	if s.ch.js == nil {
		return
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		s.log.Error("marshal message for stream", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if _, err := s.ch.js.Publish(ctx, bus.ChannelSubject(msg.GetChannel()), b, jetstream.WithMsgID(msg.GetId())); err != nil {
		s.log.Warn("stream publish failed", "channel", msg.GetChannel(), "id", msg.GetId(), "err", err)
	}
}

// auditRecord is the JSON published on the AUDIT stream.
type auditRecord struct {
	TS       string         `json:"ts"`
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Resource string         `json:"resource"`
	Outcome  string         `json:"outcome"`
	Details  map[string]any `json:"details"`
}

// publishAudit copies an audit row onto audit.<action> (best effort; the
// SQLite table is the record).
func (s *server) publishAudit(actor, action, resource, outcome string, details map[string]any) {
	if s.nc == nil || action == "" || strings.ContainsAny(action, " \t\r\n*>") {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(auditRecord{TS: nowString(), Actor: actor, Action: action, Resource: resource, Outcome: outcome, Details: details})
	if err != nil {
		return
	}
	if err := s.nc.Publish(bus.AuditSubject(action), b); err != nil {
		s.log.Debug("audit publish failed", "action", action, "err", err)
	}
}

// ---- actors and access ----

// actor is whoever acts on a channel: a user, an agent or the hub.
type actor struct {
	id      string // message "from" and member id: "user:<id>", agent id or "hub"
	p       policy.Principal
	origin  string
	node    string
	sandbox string
	paused  string // agents: the pause reason while paused, else ""
}

func userActor(u *userRow) actor {
	return actor{id: userMemberID(u.ID), p: principalOf(u), origin: originHuman}
}

var hubActor = actor{id: "hub", origin: originHub}

// auditID names the actor in audit rows (users by their bare id, as in phase 1).
func (a actor) auditID() string {
	if a.p.Kind == policy.KindUser {
		return a.p.ID
	}
	return a.id
}

func (a actor) isAdmin() bool { return a.p.Kind == policy.KindUser && a.p.Role == policy.RoleAdmin }

func userMemberID(id string) string { return "user:" + id }

func roleFor(member string) string {
	if strings.HasPrefix(member, "agent:") {
		return memberAgent
	}
	return memberHuman
}

func isDirect(ch *channelRow) bool { return ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_DIRECT }

func channelResource(ch *channelRow) policy.Resource {
	return policy.Resource{Kind: "channel", ID: ch.Name, OwnerID: ch.OwnerID}
}

// readActions are the actions that only read a channel.
var readActions = map[policy.Action]bool{
	policy.ActionChannelGet: true, policy.ActionChannelRead: true, policy.ActionBlackboardRead: true,
}

// membership reports whether id is a member of ch. Users are implicit
// members of open and system channels (not of direct channels); agents must
// join. role is "" for implicit membership.
func (s *server) membership(ctx context.Context, ch *channelRow, id string) (bool, string, error) {
	m, err := s.st.getMember(ctx, ch.Name, id)
	if err == nil {
		return true, m.Role, nil
	}
	if !errors.Is(err, errNotFound) {
		return false, "", err
	}
	if strings.HasPrefix(id, "user:") && !isDirect(ch) &&
		(ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM || ch.Policy.GetOpenToMembers()) {
		return true, "", nil
	}
	return false, "", nil
}

// checkChannel asks the policy about action on ch for a and reports whether
// a is a member. The hub itself is always allowed.
func (s *server) checkChannel(ctx context.Context, a actor, action policy.Action, ch *channelRow) (bool, policy.Decision, error) {
	if a.origin == originHub {
		return true, policy.Decision{Allow: true}, nil
	}
	if a.origin == originAgent && ch.Name == channels.Approvals {
		return false, policy.Decision{Reason: "agents may not read " + channels.Approvals}, nil
	}
	member, _, err := s.membership(ctx, ch, a.id)
	if err != nil {
		return false, policy.Decision{}, s.internalError("membership", err)
	}
	p := a.p
	p.Member = member
	p.Creator = ch.Creator == a.id
	return member, s.policy.Authorize(ctx, p, action, channelResource(ch)), nil
}

// authorizeChannel checks action on ch for a. Denials of mutating actions
// are audited (sampled). An admin reading a direct channel it is not part
// of is allowed and audited as channel.read.
func (s *server) authorizeChannel(ctx context.Context, a actor, action policy.Action, ch *channelRow, audit bool) error {
	member, d, err := s.checkChannel(ctx, a, action, ch)
	if err != nil {
		return err
	}
	if d.Allow {
		if !member && isDirect(ch) && readActions[action] {
			s.auditAdminDirectRead(ctx, a, ch, string(action))
		}
		return nil
	}
	reason := d.Reason
	if reason == "" {
		reason = "permission denied"
	}
	if audit {
		s.auditDenied(ctx, a.auditID(), string(action), "channel:"+ch.Name, map[string]any{"reason": reason})
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New(reason))
}

func (s *server) auditAdminDirectRead(ctx context.Context, a actor, ch *channelRow, via string) {
	s.audit(ctx, a.auditID(), string(policy.ActionChannelRead), "channel:"+ch.Name, outcomeOK,
		map[string]any{"via": via, "reason": "admin read of a direct channel"})
}

// loadChannel resolves a channel by name ("#name" or "dm:...").
func (s *server) loadChannel(ctx context.Context, name string) (*channelRow, error) {
	if !channels.ValidName(name) && !channels.IsDirect(name) {
		return nil, invalidArg("invalid channel name %q", name)
	}
	ch, err := s.st.getChannel(ctx, name)
	if errors.Is(err, errNotFound) {
		return nil, notFound("channel")
	}
	if err != nil {
		return nil, s.internalError("get channel", err)
	}
	return ch, nil
}

// loadAuthorized loads a channel and checks action on it.
func (s *server) loadAuthorized(ctx context.Context, a actor, name string, action policy.Action, audit bool) (*channelRow, error) {
	ch, err := s.loadChannel(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeChannel(ctx, a, action, ch, audit); err != nil {
		return nil, err
	}
	return ch, nil
}

// resolveParticipant checks that id names an existing user or a live agent
// and returns its canonical member id. Bare user ids ("usr_...") are
// accepted for "user:usr_...".
func (s *server) resolveParticipant(ctx context.Context, id string) (string, error) {
	if !strings.Contains(id, ":") && strings.HasPrefix(id, "usr_") {
		id = userMemberID(id)
	}
	if !channels.ValidParticipant(id) {
		return "", invalidArg("invalid member id %q: want agent:<name>@<node> or user:<id>", id)
	}
	if uid, ok := strings.CutPrefix(id, "user:"); ok {
		if _, err := s.st.getUser(ctx, uid); errors.Is(err, errNotFound) {
			return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("user %s not found", id))
		} else if err != nil {
			return "", s.internalError("get user", err)
		}
		return id, nil
	}
	ag, err := s.st.getAgent(ctx, id)
	if errors.Is(err, errNotFound) || (err == nil && ag.RevokedAt != "") {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %s not found", id))
	}
	if err != nil {
		return "", s.internalError("get agent", err)
	}
	return id, nil
}

// ---- conversions ----

func (s *server) channelToProto(ctx context.Context, ch *channelRow) (*mycelv1.Channel, error) {
	members, err := s.st.channelMembers(ctx, ch.Name)
	if err != nil {
		return nil, s.internalError("channel members", err)
	}
	out := &mycelv1.Channel{
		Name: ch.Name, Type: ch.Type, OwnerId: ch.OwnerID, Policy: channels.WithDefaults(ch.Policy, ch.Type),
		CreatedAt: ch.CreatedAt, ArchivedAt: ch.ArchivedAt, LastSeq: ch.LastSeq,
	}
	for _, m := range members {
		out.Members = append(out.Members, &mycelv1.Member{Id: m.Member, Role: m.Role, JoinedAt: m.JoinedAt})
	}
	return out, nil
}

// readableSet returns the channels an agent may read: its memberships plus
// open, non-direct, non-archived channels.
func (s *server) readableSet(ctx context.Context, a actor) (map[string]bool, error) {
	all, err := s.st.listChannels(ctx)
	if err != nil {
		return nil, s.internalError("list channels", err)
	}
	mine, err := s.st.memberships(ctx, a.id)
	if err != nil {
		return nil, s.internalError("memberships", err)
	}
	out := map[string]bool{}
	for _, m := range mine {
		out[m.Channel] = true
	}
	for _, ch := range all {
		if !isDirect(ch) && ch.ArchivedAt == "" && ch.Policy.GetOpenToMembers() {
			out[ch.Name] = true
		}
	}
	return out, nil
}

// visibleChannels lists the channels a may see: admins all (direct ones
// included, for audit and replay); users their channels plus open and
// system ones; agents their channels plus open (non-archived) ones.
func (s *server) visibleChannels(ctx context.Context, a actor, includeArchived bool) ([]*mycelv1.Channel, error) {
	all, err := s.st.listChannels(ctx)
	if err != nil {
		return nil, s.internalError("list channels", err)
	}
	mine, err := s.st.memberships(ctx, a.id)
	if err != nil {
		return nil, s.internalError("memberships", err)
	}
	member := map[string]bool{}
	for _, m := range mine {
		member[m.Channel] = true
	}
	var out []*mycelv1.Channel
	for _, ch := range all {
		if ch.ArchivedAt != "" && !includeArchived {
			continue
		}
		visible := member[ch.Name] || a.isAdmin()
		if !visible && !isDirect(ch) && ch.ArchivedAt == "" {
			switch a.origin {
			case originHuman:
				visible = ch.Policy.GetOpenToMembers() || ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM
			case originAgent:
				visible = ch.Policy.GetOpenToMembers()
			}
		}
		if !visible {
			continue
		}
		pc, err := s.channelToProto(ctx, ch)
		if err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

// ---- posting ----

func validMessageType(t mycelv1.MessageType) bool {
	_, ok := mycelv1.MessageType_name[int32(t)]
	return ok && t != mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED
}

// validTag: 1-32 chars of [a-z0-9-].
func validTag(t string) bool {
	if t == "" || len(t) > maxTagLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		if c := t[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validPartKind: 1-16 chars of [a-z].
func validPartKind(k string) bool {
	if k == "" || len(k) > maxPartKind {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 'a' || k[i] > 'z' {
			return false
		}
	}
	return true
}

func validatePost(req *mycelv1.PostRequest) error {
	if req == nil {
		return invalidArg("missing post")
	}
	if len(req.GetText()) > channels.MaxTextBytes {
		return invalidArg("text exceeds %d bytes", channels.MaxTextBytes)
	}
	if req.GetText() == "" && len(req.GetParts()) == 0 {
		return invalidArg("empty message: text or parts required")
	}
	if len(req.GetMentions()) > channels.MaxMentions {
		return invalidArg("at most %d mentions", channels.MaxMentions)
	}
	if t := req.GetType(); t != mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED {
		if !validMessageType(t) {
			return invalidArg("unknown message type %d", t)
		}
		if t == mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_DECISION || t == mycelv1.MessageType_MESSAGE_TYPE_APPROVAL_REQUEST {
			return connect.NewError(connect.CodePermissionDenied, errors.New("approval requests and decisions are issued by the hub"))
		}
	}
	if th := req.GetThread(); th != "" && !channels.ValidThreadID(th) {
		return invalidArg("invalid thread id %q", th)
	}
	if r := req.GetReplyTo(); r != "" && !channels.ValidMessageID(r) {
		return invalidArg("invalid reply_to %q", r)
	}
	if len(req.GetTags()) > maxTags {
		return invalidArg("at most %d tags", maxTags)
	}
	for _, t := range req.GetTags() {
		if !validTag(t) {
			return invalidArg("invalid tag %q: 1-%d chars of a-z, 0-9 and '-'", t, maxTagLen)
		}
	}
	for _, m := range req.GetMentions() {
		if !channels.ValidParticipant(m) {
			return invalidArg("invalid mention %q: want agent:<name>@<node> or user:<id>", m)
		}
	}
	if len(req.GetParts()) > maxParts {
		return invalidArg("at most %d parts", maxParts)
	}
	for _, p := range req.GetParts() {
		if !validPartKind(p.GetKind()) {
			return invalidArg("invalid part kind %q: 1-%d chars of a-z", p.GetKind(), maxPartKind)
		}
		if p.GetRef() == "" || len(p.GetRef()) > maxPartRef || !channels.IsPrintable(p.GetRef()) {
			return invalidArg("invalid part ref: 1-%d printable bytes", maxPartRef)
		}
		if len(p.GetName()) > maxPartName || !channels.IsPrintable(p.GetName()) {
			return invalidArg("invalid part name: up to %d printable bytes", maxPartName)
		}
	}
	return nil
}

// postTarget is the channel a post goes to. For the first message to a
// direct participant the channel does not exist yet: pending is set and it
// is created only once the post has passed every check.
type postTarget struct {
	ch      *channelRow
	pending bool
	members []memberRow // for a pending direct channel
}

// member reports membership, honouring a pending direct channel.
func (s *server) targetMember(ctx context.Context, t *postTarget, id string) (bool, string, error) {
	if t.pending {
		for _, m := range t.members {
			if m.Member == id {
				return true, m.Role, nil
			}
		}
		return false, "", nil
	}
	return s.membership(ctx, t.ch, id)
}

// resolvePostChannel finds the channel a post goes to; "@<participant>"
// names the direct channel with that participant.
func (s *server) resolvePostChannel(ctx context.Context, a actor, name string) (*postTarget, error) {
	target, ok := strings.CutPrefix(name, "@")
	if !ok {
		ch, err := s.loadChannel(ctx, name)
		if err != nil {
			return nil, err
		}
		return &postTarget{ch: ch}, nil
	}
	if a.origin == originHub {
		return nil, invalidArg("the hub does not send direct messages")
	}
	tgt, err := s.resolveParticipant(ctx, target)
	if err != nil {
		return nil, err
	}
	if tgt == a.id {
		return nil, invalidArg("cannot send a direct message to yourself")
	}
	dm := channels.DirectName(a.id, tgt)
	ch, err := s.st.getChannel(ctx, dm)
	if err == nil {
		return &postTarget{ch: ch}, nil
	}
	if !errors.Is(err, errNotFound) {
		return nil, s.internalError("get channel", err)
	}
	return &postTarget{
		ch: &channelRow{
			Name: dm, Type: mycelv1.ChannelType_CHANNEL_TYPE_DIRECT, Creator: a.id, CreatorOwner: a.p.OwnerID,
			Policy: &mycelv1.ChannelPolicy{}, CreatedAt: nowString(),
		},
		pending: true,
		members: []memberRow{{Member: a.id, Role: roleFor(a.id), AddedBy: addedBySelf}, {Member: tgt, Role: roleFor(tgt), AddedBy: a.auditID()}},
	}, nil
}

// createPending stores a pending direct channel.
func (s *server) createPending(ctx context.Context, a actor, t *postTarget) error {
	created, err := s.st.ensureChannel(ctx, t.ch, t.members)
	if errors.Is(err, errChannelCap) {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many channels created by this agent or owner"))
	}
	if err != nil {
		return s.internalError("create direct channel", err)
	}
	if created {
		s.audit(ctx, a.auditID(), string(policy.ActionChannelCreate), "channel:"+t.ch.Name, outcomeOK, map[string]any{"type": "direct"})
	}
	ch, err := s.st.getChannel(ctx, t.ch.Name)
	if err != nil {
		return s.internalError("get channel", err)
	}
	t.ch, t.pending = ch, false
	return nil
}

// post is the one code path for messages from humans and agents: resolve
// the channel, check membership, policy and limits, stamp identity and
// provenance, store, stream, wake inboxes and audit.
func (s *server) post(ctx context.Context, a actor, req *mycelv1.PostRequest) (*mycelv1.Message, error) {
	if err := validatePost(req); err != nil {
		return nil, err
	}
	t, err := s.resolvePostChannel(ctx, a, req.GetChannel())
	if err != nil {
		return nil, err
	}
	ch := t.ch
	if ch.ArchivedAt != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("channel is archived"))
	}
	if err := s.postFrozen(a, ch.Name); err != nil {
		s.auditDenied(ctx, a.auditID(), string(policy.ActionChannelPost), "channel:"+ch.Name, map[string]any{"reason": err.Error()})
		return nil, err
	}
	pol := channels.WithDefaults(ch.Policy, ch.Type)
	if t.pending {
		// The sender is one of the two participants.
		p := a.p
		p.Member = true
		if d := s.policy.Authorize(ctx, p, policy.ActionChannelPost, channelResource(ch)); !d.Allow {
			return nil, s.denyPost(ctx, a, ch, d.Reason)
		}
		if reason := mayPostRole(a, "", pol); reason != "" {
			return nil, s.denyPost(ctx, a, ch, reason)
		}
	} else if err := s.postAllowed(ctx, a, ch); err != nil {
		return nil, err
	}

	// Mentions: members only; in a direct channel the other participant is
	// always addressed.
	var to []string
	for _, m := range req.GetMentions() {
		id, err := s.resolveParticipant(ctx, m)
		if err != nil {
			return nil, err
		}
		member, _, err := s.targetMember(ctx, t, id)
		if err != nil {
			return nil, s.internalError("membership", err)
		}
		if !member {
			return nil, invalidArg("mention %s is not a member of %s", id, ch.Name)
		}
		if !slices.Contains(to, id) {
			to = append(to, id)
		}
	}
	if x, y, ok := channels.DirectParticipants(ch.Name); ok {
		other := x
		if other == a.id {
			other = y
		}
		if !slices.Contains(to, other) {
			to = append(to, other)
		}
	}

	// Replies stay in their parent's channel and thread.
	var parent *mycelv1.Message
	if r := req.GetReplyTo(); r != "" {
		if !t.pending {
			parent, err = s.st.getMessage(ctx, r)
		}
		if t.pending || errors.Is(err, errNotFound) || (err == nil && parent.GetChannel() != ch.Name) {
			return nil, invalidArg("reply_to %s is not a message in %s", r, ch.Name)
		}
		if err != nil {
			return nil, s.internalError("get parent", err)
		}
		if th := req.GetThread(); th != "" && th != channels.Thread("", parent) {
			return nil, invalidArg("a reply stays in its parent's thread %s", channels.Thread("", parent))
		}
	}
	thread := channels.Thread(req.GetThread(), parent)

	// Rate limits come before the hop check, so refused replies cost a token.
	if a.origin != originHub {
		if !s.ch.limiter.Allow(a.id, ch.Name, pol.GetRatePerMinute()) {
			s.log.Debug("post rate limited", "actor", a.id, "channel", ch.Name)
			return nil, connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("rate limit (%d messages per minute) reached", pol.GetRatePerMinute()))
		}
		if a.origin == originAgent && !s.ch.limiter.Allow(a.id, rateKeyAllChannels, agentGlobalRatePerMinute) {
			s.log.Debug("post rate limited (all channels)", "actor", a.id)
			return nil, connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("agent rate limit (%d messages per minute across channels) reached", agentGlobalRatePerMinute))
		}
	}
	hop, exceeded := channels.Hop(parent, pol.GetMaxHops())
	if exceeded {
		s.hopLimitEvent(ctx, ch, thread, pol.GetMaxHops())
		s.auditDenied(ctx, a.auditID(), string(policy.ActionChannelPost), "channel:"+ch.Name,
			map[string]any{"reason": "hop limit", "reply_to": req.GetReplyTo(), "max_hops": pol.GetMaxHops()})
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("hop limit (%d) reached", pol.GetMaxHops()))
	}
	if t.pending {
		if err := s.createPending(ctx, a, t); err != nil {
			return nil, err
		}
		ch = t.ch
	}

	typ := req.GetType()
	if typ == mycelv1.MessageType_MESSAGE_TYPE_UNSPECIFIED {
		typ = mycelv1.MessageType_MESSAGE_TYPE_CHAT
	}
	var parts []*mycelv1.Part
	for _, p := range req.GetParts() {
		parts = append(parts, &mycelv1.Part{Kind: p.GetKind(), Ref: p.GetRef(), Name: p.GetName()})
	}
	now := time.Now()
	msg := &mycelv1.Message{
		Id: channels.NewMessageID(now), Channel: ch.Name, Thread: thread, From: a.id, To: to, Type: typ,
		Payload: &mycelv1.Payload{Text: req.GetText(), Parts: parts, Tags: slices.Clone(req.GetTags())},
		Meta: &mycelv1.Meta{
			Hop: hop, MaxHops: pol.GetMaxHops(), ReplyTo: req.GetReplyTo(),
			Provenance: &mycelv1.Provenance{Origin: a.origin, Sandbox: a.sandbox, Node: a.node, Untrusted: a.origin == originAgent},
		},
		Ts: formatTime(now),
	}
	msg.Meta.TraceId = thread
	if thread == "" {
		msg.Meta.TraceId = msg.Id
	}
	if err := s.commitMessage(ctx, ch, msg); err != nil {
		return nil, s.internalError("store message", err)
	}
	s.audit(ctx, a.auditID(), string(policy.ActionChannelPost), "channel:"+ch.Name, outcomeOK,
		map[string]any{"message_id": msg.Id, "seq": msg.Seq, "type": strings.ToLower(strings.TrimPrefix(typ.String(), "MESSAGE_TYPE_")), "trace_id": msg.Meta.TraceId})
	return msg, nil
}

func (s *server) denyPost(ctx context.Context, a actor, ch *channelRow, reason string) error {
	s.auditDenied(ctx, a.auditID(), string(policy.ActionChannelPost), "channel:"+ch.Name, map[string]any{"reason": reason})
	return connect.NewError(connect.CodePermissionDenied, errors.New(reason))
}

// mayPostRole applies the member role and the channel's *_may_post to a
// sender other than the hub; "" means allowed.
func mayPostRole(a actor, role string, pol *mycelv1.ChannelPolicy) string {
	switch {
	case a.origin == originHub:
		return ""
	case role == memberObserver:
		return "observers may not post"
	case a.origin == originAgent && !pol.GetAgentsMayPost():
		return "agents may not post in this channel"
	case a.origin == originHuman && !pol.GetHumansMayPost():
		return "humans may not post in this channel"
	}
	return ""
}

// postAllowed checks everything an existing channel requires of a sender,
// apart from rate and hop limits: not archived, the policy (membership),
// the sender's role and the channel's *_may_post, no system channel for
// anyone but the hub, and for agents a channel budget that is not
// exhausted. Message posts and job result messages (for the job's
// submitter) both go through it. Denials are audited (sampled).
func (s *server) postAllowed(ctx context.Context, a actor, ch *channelRow) error {
	if ch.ArchivedAt != "" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("channel is archived"))
	}
	if err := s.authorizeChannel(ctx, a, policy.ActionChannelPost, ch, true); err != nil {
		return err
	}
	if a.origin == originHub {
		return nil
	}
	_, role, err := s.membership(ctx, ch, a.id)
	if err != nil {
		return s.internalError("membership", err)
	}
	if reason := mayPostRole(a, role, channels.WithDefaults(ch.Policy, ch.Type)); reason != "" {
		return s.denyPost(ctx, a, ch, reason)
	}
	if ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM {
		return s.denyPost(ctx, a, ch, "only the hub posts in system channels")
	}
	// Agents may not post into a channel whose budget is exhausted, whatever
	// their home channel (humans still may).
	if a.origin == originAgent {
		_, _, ex, err := s.channelBudget(ctx, ch.Name)
		if err != nil {
			return s.internalError("channel budget", err)
		}
		if ex {
			s.auditDenied(ctx, a.auditID(), string(policy.ActionChannelPost), "channel:"+ch.Name, map[string]any{"reason": "channel budget exhausted"})
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("channel budget exhausted"))
		}
	}
	return nil
}

// commitMessage stores msg, publishes it to the CH stream and wakes the
// channel's agents and every Watch stream.
func (s *server) commitMessage(ctx context.Context, ch *channelRow, msg *mycelv1.Message) error {
	if err := s.st.insertMessage(context.WithoutCancel(ctx), msg); err != nil {
		return err
	}
	s.publishMessage(msg)
	keys := []string{watchAll}
	members, err := s.st.channelMembers(context.WithoutCancel(ctx), ch.Name)
	if err != nil {
		s.log.Warn("channel members for notify", "channel", ch.Name, "err", err)
	}
	for _, m := range members {
		if strings.HasPrefix(m.Member, "agent:") {
			keys = append(keys, m.Member)
		}
	}
	s.ch.notify.signal(keys...)
	return nil
}

// hubEvent posts an EVENT message from the hub. hop is normally 0; a
// hop-limit event carries the channel's max_hops so that replying to it
// cannot restart the loop. Failures are logged.
func (s *server) hubEvent(ctx context.Context, channel, thread, text string, hop int32) {
	ctx = context.WithoutCancel(ctx)
	ch, err := s.st.getChannel(ctx, channel)
	if err != nil {
		s.log.Warn("hub event: channel", "channel", channel, "err", err)
		return
	}
	if ch.ArchivedAt != "" {
		return
	}
	pol := channels.WithDefaults(ch.Policy, ch.Type)
	now := time.Now()
	msg := &mycelv1.Message{
		Id: channels.NewMessageID(now), Channel: ch.Name, Thread: thread, From: hubActor.id,
		Type:    mycelv1.MessageType_MESSAGE_TYPE_EVENT,
		Payload: &mycelv1.Payload{Text: text},
		Meta:    &mycelv1.Meta{Hop: hop, MaxHops: pol.GetMaxHops(), Provenance: &mycelv1.Provenance{Origin: originHub}},
		Ts:      formatTime(now),
	}
	msg.Meta.TraceId = thread
	if thread == "" {
		msg.Meta.TraceId = msg.Id
	}
	if err := s.commitMessage(ctx, ch, msg); err != nil {
		s.log.Warn("hub event: store", "channel", channel, "err", err)
	}
}

// alert posts an EVENT about node to #alerts, at most nodeAlertsPerMinute
// per node.
func (s *server) alert(ctx context.Context, node, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if !s.ch.limiter.Allow("node:"+node, channels.Alerts, nodeAlertsPerMinute) {
		s.log.Debug("alert rate limited", "node", node, "text", text)
		return
	}
	s.hubEvent(ctx, channels.Alerts, "", text, 0)
}

// hopLimitEvent posts one EVENT per (channel, thread) when a reply chain
// hits the hop limit. The thread is always the parent chain's, and the
// store is the record of whether the event exists; at most
// hubEventsPerMinute such events per channel.
func (s *server) hopLimitEvent(ctx context.Context, ch *channelRow, thread string, maxHops int32) {
	s.ch.hopMu.Lock()
	defer s.ch.hopMu.Unlock()
	exists, err := s.st.hubEventInThread(context.WithoutCancel(ctx), ch.Name, thread)
	if err != nil {
		s.log.Warn("hop event lookup", "channel", ch.Name, "err", err)
		return
	}
	if exists {
		return
	}
	if !s.ch.limiter.Allow(rateKeyHub, ch.Name, hubEventsPerMinute) {
		s.log.Debug("hop event rate limited", "channel", ch.Name, "thread", thread)
		return
	}
	s.hubEvent(ctx, ch.Name, thread,
		fmt.Sprintf("hop limit (%d) reached in thread %s; further replies in this chain are refused", maxHops, thread), maxHops)
}

// ensureSystemChannels creates #general (open topic), #alerts and
// #approvals (system) if they are missing. ownerID is an admin.
func ensureSystemChannels(ctx context.Context, st *store, ownerID string) error {
	for _, c := range []*channelRow{
		{Name: channels.General, Type: mycelv1.ChannelType_CHANNEL_TYPE_TOPIC, Policy: &mycelv1.ChannelPolicy{OpenToMembers: true}},
		{Name: channels.Alerts, Type: mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM, Policy: &mycelv1.ChannelPolicy{OpenToMembers: true}},
		{Name: channels.Approvals, Type: mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM, Policy: &mycelv1.ChannelPolicy{OpenToMembers: true}},
	} {
		c.OwnerID, c.Creator, c.CreatedAt = ownerID, hubActor.id, nowString()
		if _, err := st.ensureChannel(ctx, c, nil); err != nil {
			return fmt.Errorf("hub: create %s: %w", c.Name, err)
		}
	}
	return nil
}

// firstAdmin returns the oldest admin user id.
func firstAdmin(ctx context.Context, st *store) (string, error) {
	users, err := st.listUsers(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Role == roleAdmin {
			return u.ID, nil
		}
	}
	return "", errors.New("hub: no admin user")
}

// ---- reading ----

func clampLimit(v int32, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(int(v), maxV)
}

func (s *server) listMessagesFor(ctx context.Context, a actor, req *mycelv1.ListMessagesRequest) (*mycelv1.ListMessagesResponse, error) {
	if req == nil {
		return nil, invalidArg("missing request")
	}
	ch, err := s.loadAuthorized(ctx, a, req.GetChannel(), policy.ActionChannelRead, false)
	if err != nil {
		return nil, err
	}
	if th := req.GetThread(); th != "" && !channels.ValidThreadID(th) {
		return nil, invalidArg("invalid thread id %q", th)
	}
	msgs, err := s.st.listMessages(ctx, ch.Name, req.GetThread(), req.GetAfterSeq(), req.GetBeforeSeq(), clampLimit(req.GetLimit(), defaultList, maxList))
	if err != nil {
		return nil, s.internalError("list messages", err)
	}
	return &mycelv1.ListMessagesResponse{Messages: msgs}, nil
}

// inboxOnce returns up to limit messages for agent after the cursor and the
// next cursor: the seq of the last message returned when limit was hit,
// otherwise the hub's max seq at query time (so non-matching traffic is
// skipped).
func (s *server) inboxOnce(ctx context.Context, agent string, after uint64, limit int) ([]*mycelv1.Message, uint64, error) {
	upto, err := s.st.maxSeq(ctx)
	if err != nil {
		return nil, after, err
	}
	if upto <= after {
		return nil, after, nil
	}
	subs, err := s.st.subscriptions(ctx, agent)
	if err != nil {
		return nil, after, err
	}
	var out []*mycelv1.Message
	cursor := after
	for {
		batch, err := s.st.inboxCandidates(ctx, agent, cursor, upto, scanBatch)
		if err != nil {
			return nil, after, err
		}
		for _, m := range batch {
			if m.GetChannel() != channels.Approvals && channels.Matches(subs[m.GetChannel()], m, agent) {
				out = append(out, m)
				if len(out) == limit {
					return out, m.GetSeq(), nil
				}
			}
		}
		if len(batch) < scanBatch {
			return out, upto, nil
		}
		cursor = batch[len(batch)-1].GetSeq()
	}
}

// inboxPull serves agent.inbox.pull, holding the request up to wait_ms when
// nothing matches. The caller has reserved the long-poll slots.
func (s *server) inboxPull(ctx context.Context, a actor, req *mycelv1.InboxPullRequest) (*mycelv1.InboxPullResponse, error) {
	// Reading the inbox is reading the channels the agent is a member of.
	p := a.p
	p.Member = true
	if d := s.policy.Authorize(ctx, p, policy.ActionChannelRead, policy.Resource{Kind: "inbox", ID: a.id, OwnerID: a.p.OwnerID}); !d.Allow {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
	}
	limit := clampLimit(req.GetLimit(), defaultInbox, maxInbox)
	wait := inboxWait(req)
	var wake <-chan struct{}
	if wait > 0 {
		var cancel func()
		wake, cancel = s.ch.notify.subscribe(a.id)
		defer cancel()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	cursor := req.GetAfterSeq()
	for {
		msgs, next, err := s.inboxOnce(ctx, a.id, cursor, limit)
		if err != nil {
			return nil, s.internalError("inbox", err)
		}
		cursor = max(next, cursor)
		if len(msgs) > 0 || wait <= 0 {
			return &mycelv1.InboxPullResponse{Messages: msgs, NextSeq: cursor}, nil
		}
		select {
		case <-wake:
		case <-timer.C:
			return &mycelv1.InboxPullResponse{NextSeq: cursor}, nil
		case <-ctx.Done():
			return &mycelv1.InboxPullResponse{NextSeq: cursor}, nil
		case <-s.ch.stop:
			return &mycelv1.InboxPullResponse{NextSeq: cursor}, nil
		}
	}
}

// inboxWait is the long-poll wait of a pull, capped at maxInboxWait.
func inboxWait(req *mycelv1.InboxPullRequest) time.Duration {
	return min(time.Duration(max(req.GetWaitMs(), 0))*time.Millisecond, maxInboxWait)
}

// summaryFor builds the late-joiner summary of ch.
func (s *server) summaryFor(ctx context.Context, ch *channelRow) (*mycelv1.ChannelSummaryResponse, error) {
	pc, err := s.channelToProto(ctx, ch)
	if err != nil {
		return nil, err
	}
	bb, err := s.st.bbGet(ctx, ch.Name, "")
	if err != nil {
		return nil, s.internalError("blackboard", err)
	}
	recent, err := s.st.listMessages(ctx, ch.Name, "", 0, 0, channels.SummaryMessages)
	if err != nil {
		return nil, s.internalError("recent messages", err)
	}
	return &mycelv1.ChannelSummaryResponse{Summary: channels.Summary(pc, bb, recent), Blackboard: bb, Recent: recent}, nil
}

// ---- blackboard ----

func validBBKey(k string) bool { return k != "" && len(k) <= maxBBKey && channels.IsPrintable(k) }

func (s *server) bbGetFor(ctx context.Context, a actor, req *mycelv1.BlackboardGetRequest) (*mycelv1.BlackboardGetResponse, error) {
	if req == nil {
		return nil, invalidArg("missing request")
	}
	ch, err := s.loadAuthorized(ctx, a, req.GetChannel(), policy.ActionBlackboardRead, false)
	if err != nil {
		return nil, err
	}
	if k := req.GetKey(); k != "" && !validBBKey(k) {
		return nil, invalidArg("invalid key")
	}
	entries, err := s.st.bbGet(ctx, ch.Name, req.GetKey())
	if err != nil {
		return nil, s.internalError("blackboard get", err)
	}
	if req.GetKey() != "" && len(entries) == 0 {
		return nil, notFound("key")
	}
	return &mycelv1.BlackboardGetResponse{Entries: entries}, nil
}

// bbWritable loads a channel for a blackboard write.
func (s *server) bbWritable(ctx context.Context, a actor, name, key string) (*channelRow, error) {
	if !validBBKey(key) {
		return nil, invalidArg("invalid key: 1-%d printable bytes", maxBBKey)
	}
	ch, err := s.loadAuthorized(ctx, a, name, policy.ActionBlackboardWrite, true)
	if err != nil {
		return nil, err
	}
	if ch.ArchivedAt != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("channel is archived"))
	}
	if ch.Type == mycelv1.ChannelType_CHANNEL_TYPE_SYSTEM && !a.isAdmin() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only admins write system channel blackboards"))
	}
	return ch, nil
}

func (s *server) bbPutFor(ctx context.Context, a actor, req *mycelv1.BlackboardPutRequest) (*mycelv1.BlackboardPutResponse, error) {
	if req == nil {
		return nil, invalidArg("missing request")
	}
	if len(req.GetValue()) > channels.MaxBlackboardValue {
		return nil, invalidArg("value exceeds %d bytes", channels.MaxBlackboardValue)
	}
	ch, err := s.bbWritable(ctx, a, req.GetChannel(), req.GetKey())
	if err != nil {
		return nil, err
	}
	e, err := s.st.bbPut(ctx, ch.Name, req.GetKey(), req.GetValue(), a.id, a.p.OwnerID, req.GetExpectVersion())
	switch {
	case errors.Is(err, errVersionConflict):
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("version conflict on %q: expected %d", req.GetKey(), req.GetExpectVersion()))
	case errors.Is(err, errBlackboardFull):
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("blackboard holds at most %d keys", maxBlackboardKeys))
	case errors.Is(err, errBlackboardQuota):
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("blackboard quota of %d bytes per owner reached", maxBlackboardBytesPerOwner))
	case err != nil:
		return nil, s.internalError("blackboard put", err)
	}
	s.audit(ctx, a.auditID(), string(policy.ActionBlackboardWrite), "channel:"+ch.Name, outcomeOK,
		map[string]any{"key": e.GetKey(), "version": e.GetVersion()})
	return &mycelv1.BlackboardPutResponse{Entry: e}, nil
}

func (s *server) bbDeleteFor(ctx context.Context, a actor, req *mycelv1.BlackboardDeleteRequest) (*mycelv1.BlackboardDeleteResponse, error) {
	if req == nil {
		return nil, invalidArg("missing request")
	}
	ch, err := s.bbWritable(ctx, a, req.GetChannel(), req.GetKey())
	if err != nil {
		return nil, err
	}
	if err := s.st.bbDelete(ctx, ch.Name, req.GetKey()); errors.Is(err, errNotFound) {
		return nil, notFound("key")
	} else if err != nil {
		return nil, s.internalError("blackboard delete", err)
	}
	s.audit(ctx, a.auditID(), string(policy.ActionBlackboardWrite), "channel:"+ch.Name, outcomeOK,
		map[string]any{"key": req.GetKey(), "deleted": true})
	return &mycelv1.BlackboardDeleteResponse{}, nil
}
