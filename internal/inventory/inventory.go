// Package inventory probes the machine a node runs on: OS, CPU, RAM, GPUs,
// Docker and Ollama presence, and available sandbox backends.
//
// Contract (owned by the orchestrator): Probe's signature is fixed. Platform
// specifics go behind build tags. Probe must never block for long: external
// commands get a short timeout and missing tools are reported as absent, not
// as errors.
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/version"
)

// ErrNothingProbed is returned when not even the basics (hostname, CPU model,
// memory) could be determined.
var ErrNothingProbed = errors.New("inventory: nothing could be determined")

// Sandbox backend names reported in Inventory.sandbox_backends.
const (
	BackendAppleVF        = "applevf"
	BackendAppleContainer = "apple-container"
	BackendLima           = "lima"
	BackendFirecracker    = "firecracker"
	BackendGVisor         = "gvisor"
	BackendWSL2GVisor     = "wsl2-gvisor"
	BackendWSL2           = "wsl2"
	BackendFake           = "fake"
)

// fakeSandbox is set by fakesandbox.go when built with the fakesandbox tag.
var fakeSandbox bool

// lateGrace is how long Probe waits for straggling sub-probes after ctx is
// done before returning what it has.
const lateGrace = 250 * time.Millisecond

// Probe collects the inventory and stamps version.Version into mycel_version. It only returns an error when nothing at all
// could be determined.
func Probe(ctx context.Context) (*mycelv1.Inventory, error) {
	var mu sync.Mutex
	inv := &mycelv1.Inventory{
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		MycelVersion: version.Version,
		Docker:       &mycelv1.ToolPresence{},
		Ollama:       &mycelv1.ToolPresence{},
	}
	set := func(f func(*mycelv1.Inventory)) {
		mu.Lock()
		defer mu.Unlock()
		f(inv)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if h, err := os.Hostname(); err == nil {
			set(func(i *mycelv1.Inventory) { i.Hostname = h })
		}
	})
	wg.Go(func() {
		v := probeOSVersion(ctx)
		set(func(i *mycelv1.Inventory) { i.OsVersion = v })
	})
	wg.Go(func() {
		n, model := probeCPU(ctx)
		set(func(i *mycelv1.Inventory) { i.CpuCount, i.CpuModel = n, model })
	})
	wg.Go(func() {
		var total uint64
		if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
			total = vm.Total
		}
		set(func(i *mycelv1.Inventory) { i.MemBytes = total })
	})
	wg.Go(func() {
		g := probeGPUs(ctx)
		set(func(i *mycelv1.Inventory) { i.Gpus = g })
	})
	wg.Go(func() {
		d := probeDocker(ctx)
		set(func(i *mycelv1.Inventory) { i.Docker = d })
	})
	wg.Go(func() {
		o := probeOllama(ctx)
		set(func(i *mycelv1.Inventory) { i.Ollama = o })
	})
	wg.Go(func() {
		b := probeBackends(ctx)
		if fakeSandbox {
			b = append(b, BackendFake)
		}
		set(func(i *mycelv1.Inventory) { i.SandboxBackends = b })
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// Sub-probes honour ctx; give them a moment to report, then return
		// whatever is in. Late writers only touch inv under mu.
		select {
		case <-done:
		case <-time.After(lateGrace):
		}
	}

	mu.Lock()
	out, _ := proto.Clone(inv).(*mycelv1.Inventory)
	mu.Unlock()

	if out.CpuCount <= 0 {
		out.CpuCount = clampInt32(runtime.NumCPU())
	}
	out.ProbedAt = time.Now().UTC().Format(time.RFC3339)
	if out.Hostname == "" && out.MemBytes == 0 && out.CpuModel == "" {
		return out, ErrNothingProbed
	}
	return out, nil
}

// Hash returns a stable hash of the inventory fields that matter for change
// detection (everything except probed_at).
func Hash(inv *mycelv1.Inventory) string {
	c := &mycelv1.Inventory{}
	if inv != nil {
		c, _ = proto.Clone(inv).(*mycelv1.Inventory)
	}
	c.ProbedAt = ""
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func probeCPU(ctx context.Context) (int32, string) {
	n := runtime.NumCPU()
	if c, err := cpu.CountsWithContext(ctx, true); err == nil && c > 0 {
		n = c
	}
	var model string
	if infos, err := cpu.InfoWithContext(ctx); err == nil {
		for _, ci := range infos {
			if ci.ModelName != "" {
				model = ci.ModelName
				break
			}
		}
	}
	return clampInt32(n), model
}

func probeOSVersion(ctx context.Context) string {
	platform, _, ver, err := host.PlatformInformationWithContext(ctx)
	if err == nil {
		if s := formatOSVersion(platform, ver); s != "" {
			return s
		}
	}
	return osVersionFallback(ctx)
}

func clampInt32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > 1<<30 {
		return 1 << 30
	}
	return int32(n)
}
