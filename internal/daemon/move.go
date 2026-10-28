package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/discovery"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Following the hub (internal/hub/moving.go, internal/discovery): a signed
// Redirect on the control subject (or on a refused bus RPC), or, after the
// hub has been unreachable for a while, an mDNS answer from a hub with the
// pinned CA fingerprint. Either way the node connects to the new bus with
// backoff while keeping its current connection and address, and persists
// the new hub and bus URLs in node.yaml only once that connection is up, so
// a redirect to an address that does not work cannot strand it. The TLS
// handshake against the pinned CA (never the system roots) is what makes
// the new address trustworthy.

// moveConfig holds the timings and the browser of hub discovery.
type moveConfig struct {
	discoverAfter   time.Duration // unreachable this long before browsing
	discoverEvery   time.Duration // browse interval while unreachable
	discoverTimeout time.Duration // one browse
	probeTimeout    time.Duration // reachability check of a discovered hub
	switchRetryMin  time.Duration // backoff of a refused connection to a new hub
	switchRetryMax  time.Duration
	browser         discovery.Browser
}

func defaultMoveConfig() moveConfig {
	return moveConfig{
		discoverAfter:   60 * time.Second,
		discoverEvery:   60 * time.Second,
		discoverTimeout: 3 * time.Second,
		probeTimeout:    5 * time.Second,
		switchRetryMin:  5 * time.Second,
		switchRetryMax:  5 * time.Minute,
	}
}

// moveNode is the node's redirect and discovery state.
type moveNode struct {
	caPEM []byte
	caFP  string
	ch    chan *mycelv1.Redirect // accepted redirects for moveLoop

	downSince time.Time // guarded by node.mu; zero while connected

	mu      sync.Mutex
	cancel  context.CancelFunc // the current switch's connect attempt
	pending *hubTarget         // the hub being switched to; nil when none
	wg      sync.WaitGroup
}

// hubTarget is a hub the node is switching to; issued is the redirect's
// issued_at (0 for a hub found by mDNS), guarded by moveNode.mu.
type hubTarget struct {
	hub, bus string
	issued   int64
}

func newMoveNode(caPEM []byte) *moveNode {
	fp, _ := identity.FingerprintPEM(caPEM)
	return &moveNode{caPEM: caPEM, caFP: fp, ch: make(chan *mycelv1.Redirect, 1), downSince: time.Now()}
}

// trackLocked records when the hub connection was lost (node.mu held).
func (m *moveNode) trackLocked(connected bool) {
	if m == nil {
		return
	}
	switch {
	case connected:
		m.downSince = time.Time{}
	case m.downSince.IsZero():
		m.downSince = time.Now()
	}
}

// acceptRedirect verifies a Redirect (from the control subject or a
// refused bus RPC) and hands it to moveLoop; it returns a control error
// text on refusal.
func (n *node) acceptRedirect(r *mycelv1.Redirect) string {
	if err := VerifyRedirect(n.mv.caPEM, r, n.cfg.now()); err != nil {
		n.log.Warn("hub redirect refused", "err", err)
		return ctlError("invalid_argument", "redirect refused: %v", err)
	}
	n.mu.Lock()
	last := n.st.RedirectIssuedAt
	n.mu.Unlock()
	n.mv.mu.Lock()
	pending := int64(0)
	if n.mv.pending != nil {
		pending = n.mv.pending.issued
	}
	n.mv.mu.Unlock()
	switch {
	case r.GetIssuedAt() < last:
		n.log.Warn("hub redirect refused: older than one already followed", "issued_at", r.GetIssuedAt(), "followed", last)
		return ctlError("failed_precondition", "redirect older than one already followed")
	case r.GetIssuedAt() == last, r.GetIssuedAt() <= pending:
		return "" // already followed, or being followed
	}
	for {
		select {
		case n.mv.ch <- r:
			return ""
		default:
		}
		select {
		case <-n.mv.ch: // a newer one replaces a pending one
		default:
		}
	}
}

// moveLoop follows redirects and, while the hub is unreachable, browses.
func (n *node) moveLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.move.discoverEvery)
	defer func() {
		t.Stop()
		n.mv.mu.Lock()
		if n.mv.cancel != nil {
			n.mv.cancel()
		}
		n.mv.mu.Unlock()
		n.mv.wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-n.mv.ch:
			n.followRedirect(ctx, r)
		case <-t.C:
			n.maybeDiscover(ctx)
		}
	}
}

// moveTo persists the new hub and bus URLs (and the redirect's issued_at
// when non-zero); it reports false when the node is closed or revoked, or
// a newer redirect was followed meanwhile.
func (n *node) moveTo(hubURL, busURL string, issuedAt int64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.state == StateRevoked {
		return false
	}
	if issuedAt != 0 && issuedAt <= n.st.RedirectIssuedAt {
		return false
	}
	st := n.st
	st.HubURL, st.BusURL = hubURL, busURL
	if issuedAt != 0 {
		st.RedirectIssuedAt = issuedAt
	}
	if err := writeStateFile(n.dir, st); err != nil {
		n.log.Error("store the new hub address", "err", err)
		return false
	}
	n.st = st
	return true
}

func (n *node) followRedirect(ctx context.Context, r *mycelv1.Redirect) {
	bus, err := NormalizeBusURL(r.GetBusUrl())
	if err != nil {
		return // VerifyRedirect already refused it
	}
	t := &hubTarget{hub: strings.TrimRight(r.GetHubUrl(), "/"), bus: bus, issued: r.GetIssuedAt()}
	n.mu.Lock()
	here := sameHub(n.st.HubURL, n.st.BusURL, t.hub, t.bus)
	n.mu.Unlock()
	if here {
		// Already on that hub (a re-signed copy): only remember issued_at.
		n.moveTo(t.hub, t.bus, t.issued)
		return
	}
	n.mv.mu.Lock()
	if p := n.mv.pending; p != nil && sameHub(p.hub, p.bus, t.hub, t.bus) {
		p.issued = max(p.issued, t.issued)
		n.mv.mu.Unlock()
		return
	}
	n.mv.mu.Unlock()
	n.log.Info("hub moved; connecting to the new hub named by its signed redirect", "hub", t.hub, "bus", t.bus)
	n.switchHub(ctx, t, "signed redirect")
}

// switchHub connects to t's bus with backoff while keeping the current
// connection and address; only once the new bus connection is up does it
// persist t and replace the current connection. A later switch cancels
// this one.
func (n *node) switchHub(ctx context.Context, t *hubTarget, why string) {
	n.mv.mu.Lock()
	if n.mv.cancel != nil {
		n.mv.cancel()
	}
	cctx, cancel := context.WithCancel(ctx)
	n.mv.cancel = cancel
	n.mv.pending = t
	n.mv.mu.Unlock()
	n.mv.wg.Go(func() {
		defer func() {
			n.mv.mu.Lock()
			if n.mv.pending == t {
				n.mv.pending = nil
			}
			n.mv.mu.Unlock()
		}()
		backoff := n.cfg.move.switchRetryMin
		for {
			nc, gen, err := n.connectBusTo(cctx, t.bus, true)
			if err == nil {
				n.mv.mu.Lock()
				issued := t.issued
				n.mv.mu.Unlock()
				if cctx.Err() != nil || !n.moveTo(t.hub, t.bus, issued) {
					nc.Close()
					return
				}
				if err := n.install(nc, gen); err != nil {
					n.log.Warn("switch hub", "err", err)
					return
				}
				n.log.Info("now connected to the hub at its new address ("+why+")", "hub", t.hub, "bus", t.bus)
				return
			}
			if cctx.Err() != nil {
				return
			}
			n.log.Warn("the new hub refused the connection; keeping the current address and retrying", "hub", t.hub, "err", err, "retry_in", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-cctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, n.cfg.move.switchRetryMax)
		}
	})
}

// maybeDiscover browses for a hub with the pinned CA fingerprint after the
// hub has been unreachable for discoverAfter, probes each answer in turn
// over TLS with the pinned CA, and switches to the first that passes.
func (n *node) maybeDiscover(ctx context.Context) {
	n.mu.Lock()
	down := !n.connected && n.state != StateRevoked && !n.closed && !n.mv.downSince.IsZero() &&
		time.Since(n.mv.downSince) >= n.cfg.move.discoverAfter
	curHub, curBus := n.st.HubURL, n.st.BusURL
	n.mu.Unlock()
	n.mv.mu.Lock()
	switching := n.mv.pending != nil
	n.mv.mu.Unlock()
	if !down || switching || n.mv.caFP == "" {
		return
	}
	b := n.cfg.move.browser
	if b == nil {
		b = discovery.Default()
	}
	hubs, err := discovery.Find(ctx, b, n.mv.caFP, n.cfg.move.discoverTimeout)
	if err != nil {
		n.log.Debug("hub discovery", "err", err)
		return
	}
	for _, h := range hubs {
		hubURL := strings.TrimRight(h.URL, "/")
		bus, err := NormalizeBusURL(h.BusURL)
		if checkHubURL(hubURL) != nil || err != nil || sameHub(hubURL, bus, curHub, curBus) {
			continue
		}
		if err := n.probeHub(ctx, hubURL); err != nil {
			n.log.Warn("discovered hub does not answer with the pinned CA; ignored", "hub", hubURL, "err", err)
			continue
		}
		n.log.Info("hub found on the local network at a new address", "hub", hubURL, "bus", bus)
		n.switchHub(ctx, &hubTarget{hub: hubURL, bus: bus}, "mDNS")
		return
	}
}

// probeHub checks that hubURL answers /healthz over TLS verified against
// the pinned CA only (never the system roots).
func (n *node) probeHub(ctx context.Context, hubURL string) error {
	n.mu.Lock()
	cert := n.cert
	n.mu.Unlock()
	hc := pinnedClient(n.pool, func() *tls.Certificate { return &cert })
	defer hc.CloseIdleConnections()
	pctx, cancel := context.WithTimeout(ctx, n.cfg.move.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, hubURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

// redirectFromReply passes the signed redirect of a refused bus RPC
// ("failed_precondition: hub moved", Mycel-Redirect header) to
// acceptRedirect.
func (n *node) redirectFromReply(h nats.Header, errText string) {
	v := h.Get(RedirectHeader)
	if v == "" || errCode(errText) != "failed_precondition" {
		return
	}
	r, err := DecodeRedirect(v)
	if err != nil {
		return
	}
	_ = n.acceptRedirect(r)
}
