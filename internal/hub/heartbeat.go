package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

const (
	// maxInventoryBytes drops heartbeats whose inventory is larger than this.
	maxInventoryBytes = 256 << 10
	// beatWriteInterval is the minimum time between two heartbeat writes for
	// one node; beats in between are coalesced in memory.
	beatWriteInterval = 2 * time.Second
	// beatFlushTick is how often coalesced beats are checked for writing.
	beatFlushTick = 500 * time.Millisecond
	// askInventoryEvery bounds how often a node is asked for the inventory
	// behind a hash the hub does not hold.
	askInventoryEvery = 5 * time.Second
)

type pendingBeat struct {
	seen string
	inv  []byte // nil: inventory unchanged
	hash string
}

// beatState coalesces heartbeat writes per node.
type beatState struct {
	mu        sync.Mutex
	known     map[string]string // node -> stored (or queued) inventory hash
	lastWrite map[string]time.Time
	pending   map[string]*pendingBeat
	warned    map[string]bool      // nodes already warned about oversized inventory
	asked     map[string]time.Time // node -> last inventory request

	stop chan struct{}
	done chan struct{}
}

func newBeatState() *beatState {
	return &beatState{
		known: map[string]string{}, lastWrite: map[string]time.Time{},
		pending: map[string]*pendingBeat{}, warned: map[string]bool{}, asked: map[string]time.Time{},
	}
}

func (b *beatState) knownHash(node string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.known[node]
	return h, ok
}

func (b *beatState) setKnown(node, hash string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.known[node]; !ok {
		b.known[node] = hash
	}
}

// forget drops all state of a removed node.
func (b *beatState) forget(node string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.known, node)
	delete(b.lastWrite, node)
	delete(b.pending, node)
	delete(b.warned, node)
	delete(b.asked, node)
}

// shouldAsk reports whether node may be asked for its inventory now, at
// most once per askInventoryEvery.
func (b *beatState) shouldAsk(node string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.asked[node]) < askInventoryEvery {
		return false
	}
	b.asked[node] = now
	return true
}

// warnOnce reports whether node has not been warned about yet.
func (b *beatState) warnOnce(node string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.warned[node] {
		return false
	}
	b.warned[node] = true
	return true
}

// record merges a beat into the node's pending state and returns the beat to
// write now, or nil if the write is deferred.
func (b *beatState) record(node string, now time.Time, inv []byte, hash string) *pendingBeat {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pending[node]
	if p == nil {
		p = &pendingBeat{}
		b.pending[node] = p
	}
	p.seen = formatTime(now)
	if inv != nil && hash != b.known[node] {
		p.inv, p.hash = inv, hash
		b.known[node] = hash
	}
	if now.Sub(b.lastWrite[node]) < beatWriteInterval {
		return nil
	}
	delete(b.pending, node)
	b.lastWrite[node] = now
	return p
}

// due takes the pending beats that may be written now (all of them if force).
func (b *beatState) due(now time.Time, force bool) map[string]*pendingBeat {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]*pendingBeat{}
	for node, p := range b.pending {
		if force || now.Sub(b.lastWrite[node]) >= beatWriteInterval {
			out[node] = p
			delete(b.pending, node)
			b.lastWrite[node] = now
		}
	}
	return out
}

func (s *server) startBeatFlusher() {
	b := s.beats
	b.stop, b.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(b.done)
		t := time.NewTicker(beatFlushTick)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case now := <-t.C:
				for node, p := range b.due(now, false) {
					s.writeBeat(node, p)
				}
			}
		}
	}()
}

// flushBeats stops the flusher and writes every pending beat.
func (s *server) flushBeats() {
	b := s.beats
	if b.stop != nil {
		close(b.stop)
		<-b.done
		b.stop = nil
	}
	for node, p := range b.due(time.Now(), true) {
		s.writeBeat(node, p)
	}
}

func (s *server) writeBeat(node string, p *pendingBeat) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	if err := s.st.touchNode(ctx, node, p.seen, p.inv, p.hash); err != nil {
		s.log.Warn("heartbeat store", "node", node, "err", err)
		if p.inv != nil {
			// Re-read the stored hash on the next beat so the inventory is retried.
			s.beats.mu.Lock()
			delete(s.beats.known, node)
			s.beats.mu.Unlock()
		}
	}
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// handleTelemetry records a heartbeat. The node name comes from the subject
// (which the bus permissions bind to the sender's certificate), never from
// the payload. Oversized inventories and malformed hashes are dropped; writes
// are coalesced to at most one per beatWriteInterval per node.
func (s *server) handleTelemetry(msg *nats.Msg) {
	name, ok := bus.NodeFromSubject(msg.Subject)
	if !ok {
		return
	}
	var hb mycelv1.Heartbeat
	if err := proto.Unmarshal(msg.Data, &hb); err != nil {
		s.log.Debug("bad heartbeat", "node", name, "err", err)
		return
	}
	if s.resendMovedRedirect(name) {
		return // a moved hub only points its nodes at the new one
	}
	var inv []byte
	var hash string
	if hb.GetInventory() != nil {
		if proto.Size(hb.GetInventory()) > maxInventoryBytes {
			if s.beats.warnOnce(name) {
				s.log.Warn("dropping heartbeat with oversized inventory", "node", name, "limit_bytes", maxInventoryBytes)
			}
			return
		}
		hash = hb.GetInventoryHash()
		if hash != "" && !isLowerHex64(hash) {
			s.log.Debug("dropping heartbeat with malformed inventory hash", "node", name)
			return
		}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(hb.GetInventory())
		if err != nil {
			return
		}
		if hash == "" {
			sum := sha256.Sum256(b)
			hash = hex.EncodeToString(sum[:])
		}
		inv = b
	}
	if _, known := s.beats.knownHash(name); !known {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		n, err := s.st.getNode(ctx, name)
		cancel()
		if errors.Is(err, errNotFound) {
			return
		}
		if err != nil {
			s.log.Warn("heartbeat node lookup", "node", name, "err", err)
			return
		}
		stored := n.InventoryHash
		if len(n.Inventory) == 0 {
			stored = ""
		}
		s.beats.setKnown(name, stored)
	}
	if h := hb.GetInventoryHash(); inv == nil && isLowerHex64(h) {
		// A node sends its inventory once per connection and then only the
		// hash. When the hub does not hold the inventory behind it (the full
		// beat came before the hub subscribed, say), a ping asks for it again.
		if known, _ := s.beats.knownHash(name); known != h && s.beats.shouldAsk(name, time.Now()) {
			s.sendCtl(context.Background(), name, &mycelv1.CtlRequest{Cmd: &mycelv1.CtlRequest_Ping{Ping: &mycelv1.Ping{}}}, false)
		}
	}
	if p := s.beats.record(name, time.Now(), inv, hash); p != nil {
		s.writeBeat(name, p)
	}
	s.handleEgress(name, &hb)
	if inv != nil {
		// A full inventory follows every (re)connect: bring the node up to
		// date (freeze state, grants) off this callback.
		s.requestSync(name)
	}
}
