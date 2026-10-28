package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// Per-node control queues. Lifecycle decisions are taken under server.life,
// but the 1 s control requests they cause are sent by one goroutine per
// node, in the order they were queued, so a node that never answers delays
// only its own control messages and never the hub's lifecycle lock.
const (
	maxCtlQueue = 1024            // queued control messages per node; more are dropped
	ctlGateWait = 2 * time.Second // longest wait for a gated item (registration reply)
)

type ctlItem struct {
	req *mycelv1.CtlRequest
	// gate, when set, holds the item until it is closed: a pause decided
	// during registration is sent only after the registration reply.
	gate <-chan struct{}
	// done, when set, receives the node's answer (or the request error)
	// after a request with timeout (default 1 s). It runs on the queue's
	// goroutine; it is not called when the hub stops meanwhile.
	done    func(*mycelv1.CtlResponse, error)
	timeout time.Duration
	// priority puts the item at the head of the queue, after other
	// priority items (the freeze state of a node that reconnected).
	priority bool
	// critical items are never dropped by the queue cap (Freeze,
	// Unfreeze, DetachGrant).
	critical bool
	// dedupe, when set, replaces a queued item with the same key in place
	// instead of queueing a second one (AttachGrant per grant, the freeze
	// state).
	dedupe string
}

// ctlKeyFreeze is the dedupe key of Freeze and Unfreeze for scope "all".
const ctlKeyFreeze = "freeze:all"

type ctlQueues struct {
	mu      sync.Mutex
	q       map[string][]ctlItem
	running map[string]bool
}

func newCtlQueues() *ctlQueues {
	return &ctlQueues{q: map[string][]ctlItem{}, running: map[string]bool{}}
}

// enqueueCtl queues a control request for node and starts its sender if
// needed. It never blocks.
func (s *server) enqueueCtl(node string, it ctlItem) {
	cq := s.ctlq
	cq.mu.Lock()
	defer cq.mu.Unlock()
	queue := cq.q[node]
	if it.dedupe != "" {
		for i := range queue {
			if queue[i].dedupe == it.dedupe {
				if old := queue[i].done; old != nil {
					go old(nil, errCtlDropped)
				}
				queue[i] = it
				return
			}
		}
	}
	if len(queue) >= maxCtlQueue && !it.critical {
		s.log.Warn("ctl queue full; control message dropped", "node", node)
		if it.done != nil {
			go it.done(nil, errCtlDropped)
		}
		return
	}
	if it.priority {
		at := 0
		for at < len(queue) && queue[at].priority {
			at++
		}
		queue = append(queue[:at], append([]ctlItem{it}, queue[at:]...)...)
	} else {
		queue = append(queue, it)
	}
	cq.q[node] = queue
	if cq.running[node] {
		return
	}
	c := s.ch
	c.workMu.Lock()
	if c.closing {
		c.workMu.Unlock()
		delete(cq.q, node)
		return
	}
	c.work.Add(1)
	c.workMu.Unlock()
	cq.running[node] = true
	go s.runCtlQueue(node)
}

// runCtlQueue sends node's queued control requests one at a time until the
// queue is empty or the hub stops.
func (s *server) runCtlQueue(node string) {
	cq := s.ctlq
	defer s.ch.work.Done()
	for {
		cq.mu.Lock()
		items := cq.q[node]
		select {
		case <-s.ch.stop:
			items = nil
		default:
		}
		if len(items) == 0 {
			delete(cq.q, node)
			delete(cq.running, node)
			cq.mu.Unlock()
			return
		}
		it := items[0]
		cq.q[node] = items[1:]
		cq.mu.Unlock()
		if it.gate != nil {
			t := time.NewTimer(ctlGateWait)
			select {
			case <-it.gate:
			case <-t.C:
			case <-s.ch.stop:
			}
			t.Stop()
		}
		if it.done != nil {
			resp, err := s.requestCtl(node, it.req, it.timeout)
			select {
			case <-s.ch.stop:
			default:
				it.done(resp, err)
			}
			continue
		}
		s.sendCtl(context.Background(), node, it.req, true)
	}
}

// errCtlDropped answers a control request that was never sent.
var errCtlDropped = errors.New("control message dropped")

// requestCtl sends a control request and decodes the node's CtlResponse.
func (s *server) requestCtl(node string, req *mycelv1.CtlRequest, timeout time.Duration) (*mycelv1.CtlResponse, error) {
	if timeout <= 0 {
		timeout = time.Second
	}
	b, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		select {
		case <-s.ch.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	m, err := s.nc.RequestWithContext(ctx, bus.NodeCtl(node), b)
	if err != nil {
		return nil, err
	}
	var resp mycelv1.CtlResponse
	if err := proto.Unmarshal(m.Data, &resp); err != nil {
		return nil, fmt.Errorf("malformed control response: %w", err)
	}
	return &resp, nil
}

// lifeFx collects the side effects of lifecycle decisions taken under
// server.life: control messages (queued in order before the lock is
// released) and EVENT posts (made after it is released).
type lifeFx struct {
	gate  <-chan struct{}
	ctls  []nodeCtl
	posts []func(context.Context)
}

type nodeCtl struct {
	node string
	req  *mycelv1.CtlRequest
}

// ctl records an AgentCtl for a's node.
func (fx *lifeFx) ctl(a *agentRow, action, reason string) {
	fx.ctls = append(fx.ctls, nodeCtl{node: a.NodeName, req: &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Agent{
		Agent: &mycelv1.AgentCtl{AgentId: a.ID, Action: action, Reason: reason},
	}}})
}

// post records work to run after the lock is released.
func (fx *lifeFx) post(f func(context.Context)) { fx.posts = append(fx.posts, f) }

// withLife runs fn under server.life, queues the control messages it
// decided before unlocking (so they leave in decision order), then runs the
// posts. Effects are applied even when fn fails part-way.
func (s *server) withLife(ctx context.Context, fx *lifeFx, fn func() error) error {
	s.life.Lock()
	err := fn()
	for _, c := range fx.ctls {
		s.enqueueCtl(c.node, ctlItem{req: c.req, gate: fx.gate})
	}
	s.life.Unlock()
	for _, p := range fx.posts {
		p(ctx)
	}
	return err
}
