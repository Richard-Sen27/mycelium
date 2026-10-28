package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

func TestConcurrentExclusiveAttach(t *testing.T) {
	b, _, _, _ := newTest(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := attach(b, fmt.Sprintf("g%d", i), &mycelv1.GpuGrant{Device: 1, Exclusive: true}); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, broker.ErrNotAllowed) {
				t.Errorf("attach: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d exclusive leases on one device", wins.Load())
	}
}

func TestDetachSparesDaemonTree(t *testing.T) {
	f := &fakeSMI{apps: map[string]string{}, killErr: map[int]error{}}
	self := os.Getpid()
	parents := map[int]int{5000: 4999, 4999: self, 4242: 1}
	b := New(Options{Runner: f.run, Killer: f.kill, SampleInterval: time.Hour,
		ParentPID: func(pid int) (int, error) {
			if p, ok := parents[pid]; ok {
				return p, nil
			}
			return 0, errors.New("unknown")
		}})
	defer func() { _ = b.Close() }()
	f.mu.Lock()
	f.apps["GPU-bbbb"] = fmt.Sprintf("4242\n%d\n5000\n", self)
	f.mu.Unlock()
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1, Exclusive: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.Detach(context.Background(), "g1", "expired"); err != nil {
		t.Fatal(err)
	}
	if k := f.killedPIDs(); !slices.Equal(k, []int{4242}) {
		t.Errorf("killed %v, want only 4242", k)
	}
}

func TestDetachListingFails(t *testing.T) {
	audit := &auditLog{}
	f := &fakeSMI{apps: map[string]string{}, killErr: map[int]error{}}
	failing := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--query-compute-apps=pid" {
			return nil, errors.New("driver gone")
		}
		return f.run(ctx, name, args...)
	}
	b := New(Options{Runner: failing, Killer: f.kill, Audit: audit.add, SampleInterval: time.Hour})
	defer func() { _ = b.Close() }()
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Fatal(err)
	}
	err := b.Detach(context.Background(), "g1", "expired")
	if err == nil || !strings.Contains(err.Error(), "driver gone") {
		t.Errorf("detach: %v", err)
	}
	found := false
	for _, op := range audit.ops() {
		if op == "kill GPU-bbbb false" {
			found = true
		}
	}
	if !found {
		t.Errorf("listing failure not audited: %v", audit.ops())
	}
	// The lease is released all the same.
	if _, err := attach(b, "g2", &mycelv1.GpuGrant{Device: 1, Exclusive: true}); err != nil {
		t.Errorf("after failed detach: %v", err)
	}
}

func TestAttachAfterClose(t *testing.T) {
	b, _, _, _ := newTest(t)
	_ = b.Close()
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err == nil {
		t.Error("attach after close")
	}
}
