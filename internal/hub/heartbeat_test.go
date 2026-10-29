package hub

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// TestHeartbeatUnknownHashAsksForInventory: a node sends its inventory once
// per connection and then only its hash. For a hash whose inventory the hub
// holds a hash-only beat asks nothing; for one it does not hold (the full
// beat was published before the hub subscribed, say) the hub pings the
// node, and the full beat that follows is stored.
func TestHeartbeatUnknownHashAsksForInventory(t *testing.T) {
	h := startTestHub(t)
	admin := h.clients(h.init.AdminAPIKey, nil)
	e := enrol(t, admin, createToken(t, admin, "n1", nil, ""), "n1")
	sn := h.connectNode(t, e)
	beat := func(hash string, inv *mycelv1.Inventory) {
		t.Helper()
		b, err := proto.Marshal(&mycelv1.Heartbeat{NodeName: "n1", Inventory: inv, InventoryHash: hash})
		if err != nil {
			t.Fatal(err)
		}
		if err := sn.nc.Publish(bus.NodeTelemetry("n1"), b); err != nil {
			t.Fatal(err)
		}
		_ = sn.nc.Flush()
	}
	pinged := func(within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case req := <-sn.ctl:
				if req.GetPing() != nil {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	cpus := func() int32 { return getNode(t, admin, "n1").GetInventory().GetCpuCount() }

	h1, h2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	beat(h1, &mycelv1.Inventory{Os: "linux", CpuCount: 4})
	eventually(t, 5*time.Second, "inventory stored", func() bool { return cpus() == 4 })
	for len(sn.ctl) > 0 {
		<-sn.ctl
	}
	beat(h1, nil)
	if pinged(time.Second) {
		t.Fatal("pinged for a hash whose inventory the hub holds")
	}

	beat(h2, nil)
	if !pinged(5 * time.Second) {
		t.Fatal("no ping for a hash whose inventory the hub lacks")
	}
	beat(h2, &mycelv1.Inventory{Os: "linux", CpuCount: 8})
	eventually(t, 5*time.Second, "new inventory stored", func() bool { return cpus() == 8 })
}
