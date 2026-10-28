package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Web Push (uiapi.go): VAPID keys in <hub dir>/vapid.json, subscriptions
// per user, and notifications that carry ids and kinds only. Sending never
// blocks a request: notifications go through a small bounded queue served
// by background workers, and a full queue drops them.

const (
	vapidFile          = "vapid.json"
	maxPushSubsPerUser = 8
	maxPushEndpoint    = 2048
	maxPushBody        = 8 << 10
	pushQueueSize      = 256
	pushWorkers        = 2
	pushSendTimeout    = 10 * time.Second
	pushTTL            = 3600 // seconds a push service keeps an undelivered notification
)

// pushEndpointOK validates a subscription endpoint (tests may replace it).
var pushEndpointOK = validPushEndpoint

// pushHTTPClient, when set by a test, replaces the SSRF-guarded client.
var pushHTTPClient webpush.HTTPClient

type pushJob struct {
	approval *mycelv1.Grant // nil: network freeze
}

type pushState struct {
	dir        string
	log        *slog.Logger
	subscriber string
	st         *store

	mu   sync.Mutex
	keys *vapidKeys

	queue     chan pushJob
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	client    webpush.HTTPClient
}

type vapidKeys struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func newPushState(dir, subscriber string, st *store, log *slog.Logger) *pushState {
	p := &pushState{
		dir: dir, log: log, subscriber: subscriber, st: st,
		queue: make(chan pushJob, pushQueueSize), stop: make(chan struct{}), client: pushHTTPClient,
	}
	if p.client == nil {
		p.client = guardedHTTPClient()
	}
	for range pushWorkers {
		p.wg.Go(p.worker)
	}
	return p
}

// close stops the workers (nil-safe).
func (p *pushState) close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stop)
		p.wg.Wait()
	})
}

// vapid returns the VAPID key pair, generating and storing it on first use.
func (p *pushState) vapid() (*vapidKeys, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keys != nil {
		return p.keys, nil
	}
	path := filepath.Join(p.dir, vapidFile)
	if b, err := os.ReadFile(path); err == nil {
		var k vapidKeys
		if err := json.Unmarshal(b, &k); err != nil || k.PublicKey == "" || k.PrivateKey == "" {
			return nil, fmt.Errorf("hub: %s is not valid", vapidFile)
		}
		p.keys = &k
		return p.keys, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, err
	}
	k := &vapidKeys{PublicKey: pub, PrivateKey: priv}
	b, err := json.Marshal(k) //nolint:gosec // written to the 0600 vapid.json only
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	p.keys = k
	return k, nil
}

// enqueue queues a notification without blocking.
func (p *pushState) enqueue(j pushJob) {
	if p == nil {
		return
	}
	select {
	case p.queue <- j:
	default:
		p.log.Warn("push queue full; notification dropped")
	}
}

func (p *pushState) worker() {
	for {
		select {
		case <-p.stop:
			return
		case j := <-p.queue:
			p.deliver(j)
		}
	}
}

// pushPayload is the notification body: ids and kinds only, never a
// reason text or a path.
func pushPayload(j pushJob) []byte {
	var v any = map[string]string{"type": "freeze"}
	if g := j.approval; g != nil {
		v = map[string]string{"type": "approval", "grant_id": g.GetId(), "kind": grantKind(g.GetSpec()), "node": g.GetNode()}
	}
	b, _ := json.Marshal(v)
	return b
}

// recipients: the admins and the node owner for an approval, every user
// for a network freeze.
func (p *pushState) recipients(ctx context.Context, j pushJob) ([]string, error) {
	if j.approval == nil {
		users, err := p.st.listUsers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(users))
		for _, u := range users {
			out = append(out, u.ID)
		}
		return out, nil
	}
	out, err := p.st.adminIDs(ctx)
	if err != nil {
		return nil, err
	}
	if n, err := p.st.getNode(ctx, j.approval.GetNode()); err == nil && n.OwnerID != "" {
		out = append(out, n.OwnerID)
	}
	return out, nil
}

func (p *pushState) deliver(j pushJob) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	users, err := p.recipients(ctx, j)
	if err != nil {
		p.log.Warn("push recipients", "err", err)
		return
	}
	subs, err := p.st.pushSubsFor(ctx, uniq(users))
	if err != nil || len(subs) == 0 {
		return
	}
	keys, err := p.vapid()
	if err != nil {
		p.log.Warn("push: vapid keys", "err", err)
		return
	}
	payload := pushPayload(j)
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		p.sendOne(ctx, keys, sub, payload)
	}
}

func (p *pushState) sendOne(ctx context.Context, keys *vapidKeys, sub *pushSubRow, payload []byte) {
	sctx, cancel := context.WithTimeout(ctx, pushSendTimeout)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(sctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient: p.client, Subscriber: p.subscriber, TTL: pushTTL, Urgency: webpush.UrgencyHigh,
			VAPIDPublicKey: keys.PublicKey, VAPIDPrivateKey: keys.PrivateKey,
		})
	if err != nil {
		// Never the endpoint URL itself (a capability): its host and the
		// cause without the URL wrapper.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		p.log.Debug("push send failed", "host", endpointHost(sub.Endpoint), "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if _, err := p.st.deletePushSub(context.WithoutCancel(ctx), "", sub.Endpoint); err != nil {
			p.log.Warn("prune push subscription", "err", err)
		}
		return
	}
	if resp.StatusCode >= 300 {
		p.log.Debug("push refused", "host", endpointHost(sub.Endpoint), "status", resp.StatusCode)
	}
}

func uniq(v []string) []string {
	seen := map[string]bool{}
	out := v[:0:0]
	for _, x := range v {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func endpointHost(e string) string {
	if u, err := url.Parse(e); err == nil {
		return u.Hostname()
	}
	return ""
}

// validPushEndpoint accepts https URLs on the default port with a DNS host
// name (push services); never an IP literal or localhost.
func validPushEndpoint(e string) bool {
	if len(e) > maxPushEndpoint {
		return false
	}
	u, err := url.Parse(e)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if net.ParseIP(h) != nil || h == "localhost" || strings.HasSuffix(h, ".localhost") || !strings.Contains(h, ".") {
		return false
	}
	return true
}

// deniedPrefixes are special-purpose ranges a push endpoint may never
// resolve to, besides loopback, private, link-local, multicast and
// unspecified addresses.
var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32",
		"fc00::/7", "::ffff:0:0/96",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// publicAddr reports whether ip is a public unicast address.
func publicAddr(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	if a.Is4In6() {
		a = a.Unmap()
		if !publicAddr(net.IP(a.AsSlice())) {
			return false
		}
	}
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// guardedHTTPClient refuses connections to loopback, private, link-local
// and unspecified addresses, so a subscription cannot point the hub at its
// own network.
func guardedHTTPClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !publicAddr(net.ParseIP(host)) {
			return fmt.Errorf("push endpoint address %s refused", host)
		}
		return nil
	}}
	tr := &http.Transport{
		DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns: 16, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: tr, Timeout: pushSendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// pushOnEvent queues the notifications an event calls for: a grant that is
// created PENDING, and a network freeze.
func (s *server) pushOnEvent(ev *mycelv1.Event) {
	switch ev.GetKind() {
	case kindGrant:
		if g := ev.GetGrant(); g != nil && ev.GetOp() == opCreated && g.GetState() == mycelv1.GrantState_GRANT_STATE_PENDING {
			s.push.enqueue(pushJob{approval: g})
		}
	case kindFreeze:
		if ev.GetId() == freezeScopeAll && ev.GetFreeze().GetAllFrozen() {
			s.push.enqueue(pushJob{})
		}
	}
}

// ---- routes ----

func (s *server) handlePushVAPID(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.sessionUser(w, r); !ok {
		return
	}
	k, err := s.push.vapid()
	if err != nil {
		s.log.Error("vapid keys", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]string{"public_key": k.PublicKey})
}

// validPushKey checks a base64url subscription key of at most n bytes.
func validPushKey(k string, n int) bool {
	if k == "" || len(k) > n*2 {
		return false
	}
	k = strings.TrimRight(k, "=")
	b, err := base64.RawURLEncoding.DecodeString(k)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(k)
	}
	return err == nil && len(b) > 0 && len(b) <= n
}

func (s *server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	var sub webpush.Subscription
	if !readAPIJSON(w, r, &sub, maxPushBody) {
		return
	}
	if !pushEndpointOK(sub.Endpoint) {
		writeAPIError(w, http.StatusBadRequest, "endpoint must be an https push service URL")
		return
	}
	if !validPushKey(sub.Keys.P256dh, 65) || !validPushKey(sub.Keys.Auth, 32) {
		writeAPIError(w, http.StatusBadRequest, "keys.p256dh and keys.auth must be base64url subscription keys")
		return
	}
	err := s.st.upsertPushSub(r.Context(), &pushSubRow{
		Endpoint: sub.Endpoint, UserID: u.ID, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth, CreatedAt: nowNano(),
	}, maxPushSubsPerUser)
	if errors.Is(err, errConflict) {
		writeAPIError(w, http.StatusConflict, "this push endpoint belongs to another user")
		return
	}
	if err != nil {
		s.log.Error("store push subscription", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.audit(r.Context(), u.ID, "push.subscribe", "user:"+u.ID, outcomeOK, map[string]any{"host": endpointHost(sub.Endpoint)})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
}

func (s *server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.sessionUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !readAPIJSON(w, r, &body, maxPushBody) {
		return
	}
	if body.Endpoint == "" || len(body.Endpoint) > maxPushEndpoint {
		writeAPIError(w, http.StatusBadRequest, "endpoint required")
		return
	}
	if _, err := s.st.deletePushSub(r.Context(), u.ID, body.Endpoint); err != nil {
		s.log.Error("delete push subscription", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// setupUI resolves the browser settings and starts passkeys and push.
func (s *server) setupUI(apiAddr string) {
	s.uiCfg = resolveUISettings(s.cfg, apiAddr)
	pk, err := newPasskeyState(s.uiCfg)
	if err != nil {
		s.log.Info("passkeys disabled", "rp_id", s.uiCfg.rpID, "reason", err.Error())
	} else {
		s.passkeys = pk
	}
	s.push = newPushState(s.dir, s.uiCfg.externalURL, s.st, s.log)
}
