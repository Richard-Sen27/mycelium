package gpu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/broker"
)

const (
	nvidiaSMI         = "nvidia-smi"
	defaultSample     = 10 * time.Second
	commandTimeout    = 15 * time.Second
	visibleDevicesEnv = "NVIDIA_VISIBLE_DEVICES"
)

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Killer kills one process.
type Killer func(pid int) error

// Options configures the broker.
type Options struct {
	// Runner runs nvidia-smi; default os/exec.
	Runner Runner
	// Killer kills processes left on a released device; default os.Process.Kill.
	Killer Killer
	// ParentPID returns a process's parent, to spare the daemon's own
	// process tree; default procfs on Linux.
	ParentPID func(pid int) (int, error)
	Audit     broker.Audit
	Ceiling   broker.Ceiling
	Logger    *slog.Logger
	// SampleInterval is the metrics period; default 10 s.
	SampleInterval time.Duration
}

// Device is one GPU as nvidia-smi reports it.
type Device struct {
	Index     int
	UUID      string
	Name      string
	MemoryMiB int64
	MIG       []MIGSlice
}

// MIGSlice is one MIG device of a GPU.
type MIGSlice struct {
	Index   int // MIG device index within its GPU
	UUID    string
	Profile string // e.g. "1g.5gb"
}

// DeviceMetrics is the latest sample of one GPU.
type DeviceMetrics struct {
	Index          int
	UtilizationPct float64 // -1 when not reported
	MemoryUsedMiB  float64 // -1 when not reported
	PowerW         float64 // -1 when not reported
	SampledAt      time.Time
}

// Broker leases GPUs. It implements broker.Broker.
type Broker struct {
	run      Runner
	kill     Killer
	parent   func(pid int) (int, error)
	audit    broker.Audit
	ceiling  broker.Ceiling
	log      *slog.Logger
	now      func() time.Time
	disabled string // why the broker can never work on this host; "" otherwise
	stop     chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	closed   bool
	frozen   bool
	leases   map[string]*lease // by grant id
	pending  map[string]*broker.Usage
	metrics  map[string]DeviceMetrics
	stopOnce sync.Once
}

var _ broker.Broker = (*Broker)(nil)

// lease is one attached grant.
type lease struct {
	grantID   string
	subject   string
	sandboxID string
	gpuUUID   string // the physical GPU
	target    string // gpuUUID or a MIG UUID
	exclusive bool
	since     time.Time     // last accounting point
	carry     time.Duration // unreported remainder below one second
}

// New starts the broker and its metrics sampler.
func New(opts Options) *Broker {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b := &Broker{
		run:     opts.Runner,
		kill:    opts.Killer,
		audit:   opts.Audit,
		ceiling: opts.Ceiling,
		log:     logger.With("component", "broker", "broker", broker.KindGPU),
		now:     time.Now,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		leases:  make(map[string]*lease),
		pending: make(map[string]*broker.Usage),
		metrics: make(map[string]DeviceMetrics),
	}
	if b.run == nil {
		b.run = execRunner
		if runtime.GOOS == "darwin" {
			b.disabled = "macOS has no GPU passthrough; GPU work goes through the ollama broker"
		}
	}
	if b.kill == nil {
		b.kill = killProcess
	}
	b.parent = opts.ParentPID
	if b.parent == nil {
		b.parent = parentPID
	}
	every := opts.SampleInterval
	if every <= 0 {
		every = defaultSample
	}
	if b.disabled != "" {
		close(b.done)
		return b
	}
	go b.sampler(every)
	return b
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output() // #nosec G204 -- fixed nvidia-smi arguments built by this package
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// Kind implements broker.Broker.
func (b *Broker) Kind() string { return broker.KindGPU }

// Available reports whether nvidia-smi lists at least one GPU.
func (b *Broker) Available(ctx context.Context) (bool, string) {
	if b.disabled != "" {
		return false, b.disabled
	}
	devs, err := b.Devices(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, "nvidia-smi not found"
		}
		return false, err.Error()
	}
	if len(devs) == 0 {
		return false, "no NVIDIA GPU found"
	}
	return true, ""
}

// Devices discovers the GPUs and their MIG slices.
func (b *Broker) Devices(ctx context.Context) ([]Device, error) {
	if b.disabled != "" {
		return nil, errors.New("gpu: " + b.disabled)
	}
	out, err := b.run(ctx, nvidiaSMI, "--query-gpu=index,uuid,name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("gpu: nvidia-smi: %w", err)
	}
	devs, err := parseDevices(out)
	if err != nil {
		return nil, err
	}
	list, err := b.run(ctx, nvidiaSMI, "-L")
	if err != nil {
		b.log.Debug("nvidia-smi -L failed; no MIG information", "err", err)
		return devs, nil
	}
	addMIG(devs, list)
	return devs, nil
}

// Attach implements broker.Broker. The endpoint is
// NVIDIA_VISIBLE_DEVICES=<GPU or MIG UUID>.
func (b *Broker) Attach(ctx context.Context, a broker.Attachment) (string, error) {
	g := a.Grant
	spec := g.GetSpec().GetGpu()
	if g.GetId() == "" || spec == nil {
		return "", fmt.Errorf("gpu: %w: not a gpu grant", broker.ErrNotAllowed)
	}
	rec := broker.Record{Broker: broker.KindGPU, GrantID: g.GetId(), Subject: g.GetSubject(),
		SandboxID: a.SandboxID, Op: "lease"}
	fail := func(err error) (string, error) {
		rec.Reason = err.Error()
		b.emit(rec)
		return "", err
	}
	if b.ceiling != nil && !b.ceiling.Offers(broker.KindGPU) {
		return fail(broker.ErrCeiling)
	}
	if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(b.now()) {
		return fail(fmt.Errorf("gpu: grant %s already expired", g.GetId()))
	}
	b.mu.Lock()
	frozen, closed := b.frozen, b.closed
	b.mu.Unlock()
	if closed {
		return fail(errors.New("gpu: broker closed"))
	}
	if frozen {
		return fail(broker.ErrFrozen)
	}
	devs, err := b.Devices(ctx)
	if err != nil {
		return fail(err)
	}
	var dev *Device
	for i := range devs {
		if devs[i].Index == int(spec.GetDevice()) {
			dev = &devs[i]
		}
	}
	if dev == nil {
		return fail(fmt.Errorf("gpu: %w: no device %d on this node", broker.ErrNotAllowed, spec.GetDevice()))
	}

	b.mu.Lock()
	target, err := b.pickTarget(dev, spec.GetMigSlice(), spec.GetExclusive(), g.GetId())
	switch {
	case b.closed:
		err = errors.New("gpu: broker closed")
	case b.frozen:
		err = broker.ErrFrozen
	}
	if err != nil {
		b.mu.Unlock()
		return fail(err)
	}
	now := b.now()
	l := &lease{
		grantID: g.GetId(), subject: g.GetSubject(), sandboxID: a.SandboxID,
		gpuUUID: dev.UUID, target: target, exclusive: spec.GetExclusive(), since: now,
	}
	if old, ok := b.leases[l.grantID]; ok {
		b.accrue(old, now)
		l.carry = old.carry
		b.keepPending(old)
	}
	b.leases[l.grantID] = l
	b.mu.Unlock()
	rec.Target, rec.Allowed = target, true
	b.emit(rec)
	b.log.Info("gpu lease attached", "grant", l.grantID, "subject", l.subject, "device", target,
		"exclusive", l.exclusive)
	return visibleDevicesEnv + "=" + target, nil
}

// pickTarget chooses the GPU or MIG slice for a grant; b.mu held.
func (b *Broker) pickTarget(dev *Device, mig string, exclusive bool, self string) (string, error) {
	if mig == "" {
		if b.conflicts(dev.UUID, dev.UUID, exclusive, self) {
			return "", fmt.Errorf("gpu: %w: device %d is leased", broker.ErrNotAllowed, dev.Index)
		}
		return dev.UUID, nil
	}
	found := false
	for _, s := range dev.MIG {
		if s.UUID != mig && !strings.EqualFold(s.Profile, mig) {
			continue
		}
		found = true
		if !b.conflicts(dev.UUID, s.UUID, exclusive, self) {
			return s.UUID, nil
		}
	}
	if !found {
		return "", fmt.Errorf("gpu: %w: no MIG slice %q on device %d", broker.ErrNotAllowed, mig, dev.Index)
	}
	return "", fmt.Errorf("gpu: %w: MIG slice %q on device %d is leased", broker.ErrNotAllowed, mig, dev.Index)
}

// conflicts reports whether a lease of target (on gpu) clashes with an
// existing lease other than self's: they overlap (same target, or one is the
// whole GPU) and either is exclusive. b.mu held.
func (b *Broker) conflicts(gpu, target string, exclusive bool, self string) bool {
	for _, l := range b.leases {
		if l.grantID == self || l.gpuUUID != gpu {
			continue
		}
		overlap := l.target == target || l.target == gpu || target == gpu
		if overlap && (exclusive || l.exclusive) {
			return true
		}
	}
	return false
}

// Detach implements broker.Broker: the lease is released and, when no other
// lease still uses the device, the processes on it are killed. Detaching an
// unknown grant is a no-op.
func (b *Broker) Detach(ctx context.Context, grantID, reason string) error {
	b.mu.Lock()
	l, ok := b.leases[grantID]
	shared := false
	if ok {
		delete(b.leases, grantID)
		b.accrue(l, b.now())
		b.keepPending(l)
		for _, o := range b.leases {
			if o.gpuUUID == l.gpuUUID && (o.target == l.target || o.target == l.gpuUUID || l.target == l.gpuUUID) {
				shared = true
			}
		}
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	b.emit(broker.Record{Broker: broker.KindGPU, GrantID: l.grantID, Subject: l.subject, SandboxID: l.sandboxID,
		Op: "release", Target: l.target, Allowed: true, Reason: reason})
	b.log.Info("gpu lease released", "grant", grantID, "device", l.target, "reason", reason, "shared", shared)
	if shared {
		// Time-sliced neighbours still run there; their processes cannot be
		// told apart from ours, so nothing is killed.
		return nil
	}
	return b.killApps(ctx, l)
}

// killApps kills the compute processes left on the lease's device, except
// the daemon's own process tree (a sandbox's processes die with the
// sandbox the daemon destroys). Kill failures are logged and skipped; a
// failed listing is audited and returned.
func (b *Broker) killApps(ctx context.Context, l *lease) error {
	out, err := b.run(ctx, nvidiaSMI, "--query-compute-apps=pid", "--id="+l.target, "--format=csv,noheader")
	if err != nil {
		b.log.Warn("gpu: cannot list compute processes", "device", l.target, "err", err)
		b.emit(broker.Record{Broker: broker.KindGPU, GrantID: l.grantID, Subject: l.subject,
			SandboxID: l.sandboxID, Op: "kill", Target: l.target, Reason: "cannot list compute processes: " + err.Error()})
		return fmt.Errorf("gpu: list compute processes on %s: %w", l.target, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 1 {
			b.log.Warn("gpu: unexpected compute-apps line", "device", l.target, "line", line)
			continue
		}
		if b.inDaemonTree(pid) {
			b.log.Info("gpu: leaving a process of the daemon's own tree", "device", l.target, "pid", pid)
			continue
		}
		rec := broker.Record{Broker: broker.KindGPU, GrantID: l.grantID, Subject: l.subject,
			SandboxID: l.sandboxID, Op: "kill", Target: l.target + " pid " + line, Allowed: true}
		if err := b.kill(pid); err != nil {
			rec.Allowed, rec.Reason = false, err.Error()
			b.log.Warn("gpu: kill failed", "device", l.target, "pid", pid, "err", err)
		} else {
			b.log.Info("gpu: killed leftover process", "device", l.target, "pid", pid)
		}
		b.emit(rec)
	}
	return nil
}

// maxTreeDepth bounds the parent walk.
const maxTreeDepth = 64

// inDaemonTree reports whether pid is the daemon or one of its descendants.
func (b *Broker) inDaemonTree(pid int) bool {
	self := os.Getpid()
	for i := 0; i < maxTreeDepth && pid > 1; i++ {
		if pid == self {
			return true
		}
		parent, err := b.parent(pid)
		if err != nil || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

// parentPID reads a process's parent from procfs (Linux); elsewhere the
// parent is unknown and only the daemon's own pid is protected.
func parentPID(pid int) (int, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	// pid (comm) state ppid ...; comm may contain spaces and parentheses.
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 2 {
		return 0, errors.New("malformed stat")
	}
	return strconv.Atoi(f[1])
}

// accrue moves elapsed lease time into carry; b.mu held.
func (b *Broker) accrue(l *lease, now time.Time) {
	if now.After(l.since) {
		l.carry += now.Sub(l.since)
	}
	l.since = now
}

// keepPending moves whole seconds of a lease into the pending usage; b.mu held.
func (b *Broker) keepPending(l *lease) {
	secs := int64(l.carry / time.Second)
	if secs == 0 {
		return
	}
	l.carry -= time.Duration(secs) * time.Second
	u := b.pending[l.grantID]
	if u == nil {
		u = &broker.Usage{GrantID: l.grantID, Subject: l.subject, Broker: broker.KindGPU}
		b.pending[l.grantID] = u
	}
	u.GPUSeconds += secs
}

// Freeze implements broker.Broker: new leases are refused. Existing leases
// stay (the daemon pauses their sandboxes) until detached.
func (b *Broker) Freeze(reason string) {
	b.mu.Lock()
	b.frozen = true
	b.mu.Unlock()
	b.log.Warn("gpu broker frozen", "reason", reason)
}

// Unfreeze implements broker.Broker.
func (b *Broker) Unfreeze() {
	b.mu.Lock()
	b.frozen = false
	b.mu.Unlock()
	b.log.Info("gpu broker unfrozen")
}

// Usage implements broker.Broker: whole gpu_seconds per grant since the last
// call.
func (b *Broker) Usage() []broker.Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for _, l := range b.leases {
		b.accrue(l, now)
		b.keepPending(l)
	}
	out := make([]broker.Usage, 0, len(b.pending))
	for _, u := range b.pending {
		out = append(out, *u)
	}
	clear(b.pending)
	return out
}

// Metrics returns the latest sample per GPU, keyed by the GPU index.
func (b *Broker) Metrics() map[string]DeviceMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]DeviceMetrics, len(b.metrics))
	for k, v := range b.metrics {
		out[k] = v
	}
	return out
}

// Close stops the sampler. Leases are left to the daemon's Detach calls.
func (b *Broker) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.stopOnce.Do(func() { close(b.stop) })
	<-b.done
	return nil
}

func (b *Broker) sampler(every time.Duration) {
	defer close(b.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-b.stop
		cancel()
	}()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		b.sample(ctx)
		select {
		case <-b.stop:
			return
		case <-t.C:
		}
	}
}

// sample takes one metrics sample.
func (b *Broker) sample(ctx context.Context) {
	out, err := b.run(ctx, nvidiaSMI, "--query-gpu=index,utilization.gpu,memory.used,power.draw",
		"--format=csv,noheader,nounits")
	if err != nil {
		b.log.Debug("gpu metrics sample failed", "err", err)
		return
	}
	ms := parseMetrics(out, b.now())
	b.mu.Lock()
	b.metrics = ms
	b.mu.Unlock()
}

func (b *Broker) emit(rec broker.Record) {
	if b.audit != nil {
		b.audit(rec)
	}
}
