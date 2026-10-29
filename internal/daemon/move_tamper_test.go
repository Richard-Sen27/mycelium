package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// flipBit returns a copy of r with one bit of its signature flipped.
func flipBit(r *mycelv1.Redirect, bit int) *mycelv1.Redirect {
	c := proto.Clone(r).(*mycelv1.Redirect)
	c.Signature[bit/8%len(c.Signature)] ^= 1 << (bit % 8)
	return c
}

// TestNodeIgnoresTamperedRedirect (phase 7 acceptance, unit level): a
// redirect signed by the pinned CA but with one bit of its signature
// flipped is refused on the control subject and ignored in a refused bus
// RPC; the node keeps its hub. The untampered redirect is then followed.
func TestNodeIgnoresTamperedRedirect(t *testing.T) {
	h := newFakeHub(t)
	dir := joinNode(t, h, "n1")
	r := startRun(t, dir)
	waitConnected(t, r.addr)
	nb := secondBus(t, h)
	hc := h.hubConn()
	good := signed(t, h.ca, "https://new.test:7777", nb.ClientURL(), time.Now().Unix())

	for _, bit := range []int{0, 7, 8 * len(good.Signature) / 2, 8*len(good.Signature) - 1} {
		resp := ctl(t, hc, "n1", redirectCtl(flipBit(good, bit)))
		if resp.GetOk() || !strings.Contains(resp.GetError(), "redirect refused") {
			t.Fatalf("signature bit %d flipped: %v, want refused", bit, resp)
		}
	}

	// The same on the bus RPC path (a moved hub's failed_precondition).
	n, err := loadNode(dir, discardLogger(), testRunConfig(newFakeProbe(), &stateLog{}))
	if err != nil {
		t.Fatal(err)
	}
	v, err := EncodeRedirect(flipBit(good, 3))
	if err != nil {
		t.Fatal(err)
	}
	hdr := nats.Header{}
	hdr.Set(RedirectHeader, v)
	n.redirectFromReply(hdr, "failed_precondition: hub moved to https://new.test:7777")
	select {
	case got := <-n.mv.ch:
		t.Fatalf("tampered redirect handed on: %v", got)
	default:
	}

	if st, _ := ReadState(dir); st.HubURL != h.http.URL || st.RedirectIssuedAt != 0 {
		t.Fatalf("state changed by tampered redirects: %+v", st)
	}
	if !containsString(h.busServer().ConnectedNodes(), "n1") {
		t.Fatal("node left its hub after tampered redirects")
	}

	if resp := ctl(t, hc, "n1", redirectCtl(good)); !resp.GetOk() {
		t.Fatalf("untampered redirect: %v", resp)
	}
	waitFor(t, 10*time.Second, "node follows the untampered redirect", func() bool {
		st, _ := ReadState(dir)
		return st.HubURL == "https://new.test:7777"
	})
}
