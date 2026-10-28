package gpu

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

const (
	queryOut = "0, GPU-aaaa, NVIDIA A100-SXM4-40GB, 40960\n1, GPU-bbbb, NVIDIA GeForce RTX 4090, 24564\n"
	listOut  = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaaa)
  MIG 1g.5gb      Device  0: (UUID: MIG-a0)
  MIG 1g.5gb      Device  1: (UUID: MIG-a1)
  MIG 3g.20gb     Device  2: (UUID: MIG-a2)
GPU 1: NVIDIA GeForce RTX 4090 (UUID: GPU-bbbb)
`
	metricsOut = "0, 45, 1234, 250.12\n1, [N/A], 512, [Not Supported]\n"
)

type fakeSMI struct {
	mu      sync.Mutex
	calls   [][]string
	apps    map[string]string // --id → compute-apps output
	missing bool
	killed  []int
	killErr map[int]error
}

func (f *fakeSMI) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.missing {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	if name != "nvidia-smi" || len(args) == 0 {
		return nil, errors.New("unexpected command")
	}
	switch args[0] {
	case "-L":
		return []byte(listOut), nil
	case "--query-gpu=index,uuid,name,memory.total":
		return []byte(queryOut), nil
	case "--query-gpu=index,utilization.gpu,memory.used,power.draw":
		return []byte(metricsOut), nil
	case "--query-compute-apps=pid":
		id := strings.TrimPrefix(args[1], "--id=")
		return []byte(f.apps[id]), nil
	}
	return nil, fmt.Errorf("unexpected args %v", args)
}

func (f *fakeSMI) kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, pid)
	return f.killErr[pid]
}

func (f *fakeSMI) killedPIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.killed)
}

type fakeCeiling struct{ offers atomic.Bool }

func (c *fakeCeiling) Offers(kind string) bool { return kind == broker.KindGPU && c.offers.Load() }
func (c *fakeCeiling) Active() bool            { return true }

type auditLog struct {
	mu   sync.Mutex
	recs []broker.Record
}

func (a *auditLog) add(r broker.Record) {
	a.mu.Lock()
	a.recs = append(a.recs, r)
	a.mu.Unlock()
}

func (a *auditLog) ops() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.recs {
		out = append(out, fmt.Sprintf("%s %s %v", r.Op, r.Target, r.Allowed))
	}
	return out
}

func newTest(t *testing.T) (*Broker, *fakeSMI, *fakeCeiling, *auditLog) {
	t.Helper()
	f := &fakeSMI{apps: map[string]string{}, killErr: map[int]error{}}
	c := &fakeCeiling{}
	c.offers.Store(true)
	a := &auditLog{}
	b := New(Options{Runner: f.run, Killer: f.kill, Audit: a.add, Ceiling: c, SampleInterval: time.Hour})
	t.Cleanup(func() { _ = b.Close() })
	return b, f, c, a
}

func attach(b *Broker, id string, spec *mycelv1.GpuGrant) (string, error) {
	return b.Attach(context.Background(), broker.Attachment{
		Grant: &mycelv1.Grant{
			Id: id, Subject: "agent:" + id + "@gpu-box",
			Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Gpu{Gpu: spec}},
		},
		SandboxID: "sbx_" + id,
	})
}

func TestDiscovery(t *testing.T) {
	b, f, _, _ := newTest(t)
	devs, err := b.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("devices %+v", devs)
	}
	d0, d1 := devs[0], devs[1]
	if d0.Index != 0 || d0.UUID != "GPU-aaaa" || d0.Name != "NVIDIA A100-SXM4-40GB" || d0.MemoryMiB != 40960 {
		t.Errorf("d0 %+v", d0)
	}
	if len(d0.MIG) != 3 || d0.MIG[2] != (MIGSlice{Index: 2, UUID: "MIG-a2", Profile: "3g.20gb"}) {
		t.Errorf("mig %+v", d0.MIG)
	}
	if d1.UUID != "GPU-bbbb" || len(d1.MIG) != 0 {
		t.Errorf("d1 %+v", d1)
	}
	if ok, why := b.Available(context.Background()); !ok {
		t.Errorf("available: %s", why)
	}
	f.mu.Lock()
	f.missing = true
	f.mu.Unlock()
	if ok, why := b.Available(context.Background()); ok || why != "nvidia-smi not found" {
		t.Errorf("missing: %v %q", ok, why)
	}
	if _, err := parseDevices([]byte("x, y\n")); err == nil {
		t.Error("bad line accepted")
	}
	// Older drivers print the MIG UUID as MIG-GPU-<uuid>/gi/ci.
	devs = []Device{{Index: 0, UUID: "GPU-x"}}
	addMIG(devs, []byte("GPU 0: T (UUID: GPU-x)\n  MIG 2g.10gb Device 0: (UUID: MIG-GPU-x/1/0)\n"))
	if len(devs[0].MIG) != 1 || devs[0].MIG[0].UUID != "MIG-GPU-x/1/0" || devs[0].MIG[0].Profile != "2g.10gb" {
		t.Errorf("old format %+v", devs[0].MIG)
	}
}

func TestDefaultRunnerOnMac(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	b := New(Options{})
	defer func() { _ = b.Close() }()
	ok, why := b.Available(context.Background())
	if ok || !strings.Contains(why, "macOS") {
		t.Errorf("%v %q", ok, why)
	}
	if b.Kind() != broker.KindGPU {
		t.Error(b.Kind())
	}
}

func TestExclusiveAndTimeSliced(t *testing.T) {
	b, _, _, a := newTest(t)
	ep, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1, Exclusive: true})
	if err != nil || ep != "NVIDIA_VISIBLE_DEVICES=GPU-bbbb" {
		t.Fatalf("%q %v", ep, err)
	}
	if _, err := attach(b, "g2", &mycelv1.GpuGrant{Device: 1}); !errors.Is(err, broker.ErrNotAllowed) {
		t.Errorf("second lease on exclusive device: %v", err)
	}
	// Re-attaching the same grant replaces its own lease.
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Errorf("re-attach: %v", err)
	}
	if _, err := attach(b, "g2", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Errorf("time-sliced share: %v", err)
	}
	if _, err := attach(b, "g3", &mycelv1.GpuGrant{Device: 1, Exclusive: true}); err == nil {
		t.Error("exclusive on a shared device")
	}
	if _, err := attach(b, "g4", &mycelv1.GpuGrant{Device: 7}); err == nil {
		t.Error("unknown device")
	}

	// MIG: by profile picks a free slice, by UUID exactly that one.
	ep, err = attach(b, "m1", &mycelv1.GpuGrant{Device: 0, MigSlice: "1g.5gb", Exclusive: true})
	if err != nil || ep != "NVIDIA_VISIBLE_DEVICES=MIG-a0" {
		t.Fatalf("%q %v", ep, err)
	}
	ep, err = attach(b, "m2", &mycelv1.GpuGrant{Device: 0, MigSlice: "1g.5gb", Exclusive: true})
	if err != nil || ep != "NVIDIA_VISIBLE_DEVICES=MIG-a1" {
		t.Fatalf("%q %v", ep, err)
	}
	if _, err := attach(b, "m3", &mycelv1.GpuGrant{Device: 0, MigSlice: "1g.5gb", Exclusive: true}); err == nil {
		t.Error("third 1g.5gb slice")
	}
	if _, err := attach(b, "m4", &mycelv1.GpuGrant{Device: 0, MigSlice: "MIG-a2"}); err != nil {
		t.Errorf("slice by uuid: %v", err)
	}
	if _, err := attach(b, "m5", &mycelv1.GpuGrant{Device: 0, MigSlice: "7g.80gb"}); err == nil {
		t.Error("unknown profile")
	}
	// The whole GPU overlaps its exclusive slices.
	if _, err := attach(b, "w", &mycelv1.GpuGrant{Device: 0}); err == nil {
		t.Error("whole GPU while slices are exclusively leased")
	}
	ops := a.ops()
	if !slices.Contains(ops, "lease GPU-bbbb true") || !slices.Contains(ops, "lease  false") {
		t.Errorf("audit %v", ops)
	}
}

func TestDetachKills(t *testing.T) {
	b, f, _, a := newTest(t)
	f.apps["GPU-bbbb"] = "4242\n4343\n"
	f.killErr[4343] = errors.New("no such process")
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := attach(b, "g2", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Fatal(err)
	}
	// A time-sliced neighbour still uses the device: nothing is killed.
	if err := b.Detach(context.Background(), "g1", "expired"); err != nil {
		t.Fatal(err)
	}
	if k := f.killedPIDs(); len(k) != 0 {
		t.Errorf("killed %v while shared", k)
	}
	if err := b.Detach(context.Background(), "g2", "revoked"); err != nil {
		t.Fatal(err)
	}
	if k := f.killedPIDs(); !slices.Equal(k, []int{4242, 4343}) {
		t.Errorf("killed %v", k)
	}
	ops := a.ops()
	if !slices.Contains(ops, "kill GPU-bbbb pid 4242 true") || !slices.Contains(ops, "kill GPU-bbbb pid 4343 false") ||
		!slices.Contains(ops, "release GPU-bbbb true") {
		t.Errorf("audit %v", ops)
	}
	// The device is free again; unknown grants detach quietly.
	if _, err := attach(b, "g3", &mycelv1.GpuGrant{Device: 1, Exclusive: true}); err != nil {
		t.Errorf("after release: %v", err)
	}
	if err := b.Detach(context.Background(), "nope", ""); err != nil {
		t.Error(err)
	}
	// MIG slices are queried by their own UUID.
	f.mu.Lock()
	f.apps["MIG-a0"] = "99\n"
	f.mu.Unlock()
	if _, err := attach(b, "m", &mycelv1.GpuGrant{Device: 0, MigSlice: "MIG-a0"}); err != nil {
		t.Fatal(err)
	}
	_ = b.Detach(context.Background(), "m", "")
	if k := f.killedPIDs(); k[len(k)-1] != 99 {
		t.Errorf("killed %v", k)
	}
}

func TestMetrics(t *testing.T) {
	b, _, _, _ := newTest(t)
	b.sample(context.Background())
	m := b.Metrics()
	if len(m) != 2 {
		t.Fatalf("%+v", m)
	}
	if g := m["0"]; g.Index != 0 || g.UtilizationPct != 45 || g.MemoryUsedMiB != 1234 || g.PowerW != 250.12 || g.SampledAt.IsZero() {
		t.Errorf("gpu0 %+v", g)
	}
	if g := m["1"]; g.UtilizationPct != -1 || g.MemoryUsedMiB != 512 || g.PowerW != -1 {
		t.Errorf("gpu1 %+v", g)
	}
}

func TestSamplerRuns(t *testing.T) {
	f := &fakeSMI{apps: map[string]string{}}
	b := New(Options{Runner: f.run, SampleInterval: 5 * time.Millisecond})
	deadline := time.Now().Add(5 * time.Second)
	for len(b.Metrics()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no sample")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSeconds(t *testing.T) {
	b, _, _, _ := newTest(t)
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	b.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Fatal(err)
	}
	advance(2500 * time.Millisecond)
	us := b.Usage()
	if len(us) != 1 || us[0].GPUSeconds != 2 || us[0].GrantID != "g1" || us[0].Broker != broker.KindGPU || us[0].Subject != "agent:g1@gpu-box" {
		t.Fatalf("usage %+v", us)
	}
	advance(600 * time.Millisecond) // 0.5 carried + 0.6 = 1.1
	if us := b.Usage(); len(us) != 1 || us[0].GPUSeconds != 1 {
		t.Errorf("carry %+v", us)
	}
	advance(3 * time.Second)
	_ = b.Detach(context.Background(), "g1", "")
	if us := b.Usage(); len(us) != 1 || us[0].GPUSeconds != 3 {
		t.Errorf("after detach %+v", us)
	}
	if us := b.Usage(); len(us) != 0 {
		t.Errorf("reported twice %+v", us)
	}
}

func TestFreezeAndCeiling(t *testing.T) {
	b, _, c, _ := newTest(t)
	b.Freeze("network frozen")
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); !errors.Is(err, broker.ErrFrozen) {
		t.Errorf("frozen: %v", err)
	}
	b.Unfreeze()
	if _, err := attach(b, "g1", &mycelv1.GpuGrant{Device: 1}); err != nil {
		t.Errorf("unfrozen: %v", err)
	}
	c.offers.Store(false)
	if _, err := attach(b, "g2", &mycelv1.GpuGrant{Device: 1}); !errors.Is(err, broker.ErrCeiling) {
		t.Errorf("ceiling: %v", err)
	}
	if _, err := b.Attach(context.Background(), broker.Attachment{Grant: &mycelv1.Grant{Id: "x"}}); err == nil {
		t.Error("non-gpu grant accepted")
	}
}
