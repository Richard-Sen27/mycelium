package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
	"github.com/richard-sen27/mycelium/internal/broker/docker"
	"github.com/richard-sen27/mycelium/internal/broker/gpu"
	"github.com/richard-sen27/mycelium/internal/broker/ollama"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// EnvOllamaUpstream overrides the model endpoint the Ollama broker fronts.
const EnvOllamaUpstream = "MYCEL_OLLAMA_UPSTREAM"

const (
	defaultOllamaUpstream = "http://127.0.0.1:11434"
	// dockerSocketDir is the brokered Docker sockets' directory under NodeDir.
	dockerSocketDir = "dk"
	// maxSocketPath keeps unix socket paths below the 104-byte limit of macOS.
	maxSocketPath     = 90
	brokerRPCTimeout  = 5 * time.Second
	brokerAvailWait   = 2 * time.Second
	brokerDetachWait  = 10 * time.Second
	brokerFinalReport = 3 * time.Second
	maxPendingUsage   = 1024
	sampleGrantSocket = "grt_01ARZ3NDEKTSV4RRFFQ69G5FAV.sock"
)

func ollamaUpstreamDefault() string {
	if v := os.Getenv(EnvOllamaUpstream); v != "" {
		return v
	}
	return defaultOllamaUpstream
}

// availability is a cached Available answer of one broker kind.
type availability struct {
	ok  bool
	why string
	at  time.Time
}

// brokerSet holds the node's resource brokers. Docker runs twice: on unix
// sockets for trusted-host agents and on loopback TCP for sandboxes, which
// reach it through the egress proxy alias docker.mycel.internal:2375 (on
// Windows both are the same TCP broker).
type brokerSet struct {
	n          *node
	log        *slog.Logger
	ollama     broker.Broker
	ollamaAddr string // the Ollama broker's listen address, "" when it is down
	gpu        broker.Broker
	dockerHost broker.Broker // trusted-host agents
	dockerSbx  broker.Broker // sandboxes (TCP)

	mu      sync.Mutex
	pending map[string]*broker.Usage // unreported usage by grant id
	avail   map[string]availability  // by kind
	probing map[string]bool
}

// deadBroker stands in for a broker that could not start: it stays
// registered and answers Attach with the reason.
type deadBroker struct {
	kind, reason string
}

func (d *deadBroker) Kind() string                             { return d.kind }
func (d *deadBroker) Available(context.Context) (bool, string) { return false, d.reason }
func (d *deadBroker) Attach(context.Context, broker.Attachment) (string, error) {
	return "", errors.New(d.reason)
}
func (d *deadBroker) Detach(context.Context, string, string) error { return nil }
func (d *deadBroker) Freeze(string)                                {}
func (d *deadBroker) Unfreeze()                                    {}
func (d *deadBroker) Usage() []broker.Usage                        { return nil }
func (d *deadBroker) Close() error                                 { return nil }

// dockerSocketDirFor picks the brokered Docker socket directory: <NodeDir>/dk,
// or on macOS <tmp>/mycel-dk-<uid> when the socket path would be too long.
func dockerSocketDirFor(nodeDir string) string {
	dir := filepath.Join(nodeDir, dockerSocketDir)
	if runtime.GOOS == "darwin" && len(filepath.Join(dir, sampleGrantSocket)) > maxSocketPath {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("mycel-dk-%d", os.Getuid()))
	}
	return dir
}

// startBrokers starts the ollama, gpu and docker brokers. A broker that
// cannot start is replaced by a deadBroker.
func startBrokers(n *node, ceiling broker.Ceiling, gpuDevices func(subject, sandboxID string) []string) *brokerSet {
	bs := &brokerSet{n: n, log: n.log.With("component", "brokers"), pending: map[string]*broker.Usage{},
		avail: map[string]availability{}, probing: map[string]bool{}}
	logger := n.log
	if ob, err := ollama.New(ollama.Options{Upstream: n.cfg.ollamaUpstream, Audit: bs.audit, Ceiling: ceiling, Logger: logger}); err != nil {
		bs.log.Warn("ollama broker did not start", "err", err)
		bs.ollama = &deadBroker{kind: broker.KindOllama, reason: "ollama broker did not start: " + err.Error()}
	} else {
		bs.ollama, bs.ollamaAddr = ob, ob.Addr().String()
	}
	bs.gpu = gpu.New(gpu.Options{Runner: n.cfg.gpuRunner, Audit: bs.audit, Ceiling: ceiling, Logger: logger})
	newDocker := func(network string) broker.Broker {
		db, err := docker.New(docker.Options{Engine: n.cfg.dockerEngine, SocketDir: dockerSocketDirFor(n.dir),
			Network: network, GPUDevices: gpuDevices, Audit: bs.audit, Ceiling: ceiling, Logger: logger})
		if err != nil {
			bs.log.Warn("docker broker did not start", "network", network, "err", err)
			return &deadBroker{kind: broker.KindDockerHost, reason: "docker broker did not start: " + err.Error()}
		}
		return db
	}
	bs.dockerHost = newDocker("")
	if runtime.GOOS == "windows" {
		bs.dockerSbx = bs.dockerHost
	} else {
		bs.dockerSbx = newDocker("tcp")
	}
	return bs
}

// all lists every distinct broker.
func (bs *brokerSet) all() []broker.Broker {
	out := []broker.Broker{bs.ollama, bs.gpu, bs.dockerHost}
	if bs.dockerSbx != bs.dockerHost {
		out = append(out, bs.dockerSbx)
	}
	return out
}

// forGrant is the broker serving a grant of kind for subject: sandboxed
// subjects (jobs) get the TCP Docker broker.
func (bs *brokerSet) forGrant(kind, subject string) broker.Broker {
	switch kind {
	case broker.KindOllama:
		return bs.ollama
	case broker.KindGPU:
		return bs.gpu
	case broker.KindDockerHost:
		if strings.HasPrefix(subject, "job:") {
			return bs.dockerSbx
		}
		return bs.dockerHost
	}
	return nil
}

// ---- availability ----

// probeAll asks every broker kind whether its resource is there, in
// parallel, and caches the answers.
func (bs *brokerSet) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, k := range []string{broker.KindOllama, broker.KindGPU, broker.KindDockerHost} {
		wg.Go(func() { bs.probe(ctx, k) })
	}
	wg.Wait()
}

func (bs *brokerSet) probe(ctx context.Context, kind string) {
	pctx, cancel := context.WithTimeout(ctx, brokerAvailWait)
	ok, why := bs.forGrant(kind, "").Available(pctx)
	cancel()
	bs.mu.Lock()
	prev, had := bs.avail[kind]
	bs.avail[kind] = availability{ok: ok, why: why, at: time.Now()}
	bs.mu.Unlock()
	if !had || prev.ok != ok {
		bs.log.Info("broker availability", "broker", kind, "available", ok, "reason", why)
	}
}

// available returns the cached answer for kind. An unavailable or missing
// answer starts a fresh probe in the background, so attaching never waits
// for a slow resource.
func (bs *brokerSet) available(kind string) (bool, string) {
	bs.mu.Lock()
	a, had := bs.avail[kind]
	start := (!had || !a.ok) && !bs.probing[kind]
	if start {
		bs.probing[kind] = true
	}
	bs.mu.Unlock()
	if start {
		go func() {
			bs.probe(context.Background(), kind)
			bs.mu.Lock()
			delete(bs.probing, kind)
			bs.mu.Unlock()
		}()
	}
	if !had {
		return false, "availability not probed yet"
	}
	return a.ok, a.why
}

// probeLoop refreshes the availability cache every brokerProbeEvery.
func (bs *brokerSet) probeLoop(ctx context.Context) {
	t := time.NewTicker(bs.n.cfg.brokerProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bs.probeAll(ctx)
		}
	}
}

// ---- audit, freeze, close ----

// audit logs every brokered request. Records carry no credentials.
func (bs *brokerSet) audit(rec broker.Record) {
	bs.log.Info("broker request", "broker", rec.Broker, "grant", rec.GrantID, "subject", rec.Subject,
		"sandbox", rec.SandboxID, "op", rec.Op, "target", rec.Target, "allowed", rec.Allowed,
		"reason", rec.Reason, "bytes", rec.Bytes, "tokens", rec.Tokens)
}

func (bs *brokerSet) freeze(reason string) {
	for _, b := range bs.all() {
		b.Freeze(reason)
	}
}

func (bs *brokerSet) unfreeze() {
	for _, b := range bs.all() {
		b.Unfreeze()
	}
}

func (bs *brokerSet) close() {
	for _, b := range bs.all() {
		if err := b.Close(); err != nil {
			bs.log.Warn("close broker", "broker", b.Kind(), "err", err)
		}
	}
}

// ---- usage ----

// usageLoop reports broker usage every brokerUsageEvery, batched per grant.
func (bs *brokerSet) usageLoop(ctx context.Context) {
	t := time.NewTicker(bs.n.cfg.brokerUsageEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bs.reportUsage(ctx)
		}
	}
}

// finalUsage sends what is left at shutdown, bounded by brokerFinalReport.
func (bs *brokerSet) finalUsage() {
	ctx, cancel := context.WithTimeout(context.Background(), brokerFinalReport)
	defer cancel()
	bs.reportUsage(ctx)
}

// collect merges the brokers' increments into the pending batch.
func (bs *brokerSet) collect() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for _, b := range bs.all() {
		for _, u := range b.Usage() {
			p := bs.pending[u.GrantID]
			if p == nil {
				if len(bs.pending) >= maxPendingUsage {
					bs.log.Warn("broker usage dropped: too many unreported grants", "grant", u.GrantID)
					continue
				}
				c := u
				bs.pending[u.GrantID] = &c
				continue
			}
			p.Tokens += u.Tokens
			p.Requests += u.Requests
			p.GPUSeconds += u.GPUSeconds
			if u.Model != "" {
				p.Model = u.Model
			}
		}
	}
}

// reportUsage sends one broker.usage per grant with pending usage. A
// transport failure ends the round and keeps the usage for the next one; a
// "deny" reply detaches the grant locally.
func (bs *brokerSet) reportUsage(ctx context.Context) {
	bs.collect()
	bs.mu.Lock()
	batch := make([]broker.Usage, 0, len(bs.pending))
	for _, u := range bs.pending {
		batch = append(batch, *u)
	}
	bs.mu.Unlock()
	for _, u := range batch {
		if bs.n.busConn() == nil || ctx.Err() != nil {
			return
		}
		req := &mycelv1.BrokerUsageRequest{GrantId: u.GrantID, Subject: u.Subject, Broker: u.Broker,
			Tokens: u.Tokens, Requests: u.Requests, GpuSeconds: u.GPUSeconds, Model: u.Model}
		resp := &mycelv1.BrokerUsageResponse{}
		rctx, cancel := context.WithTimeout(ctx, brokerRPCTimeout)
		err := bs.n.hubRPC(rctx, bus.RPCBrokerUsage, req, resp)
		cancel()
		var he *hubError
		if err != nil && (!errors.As(err, &he) || retryableHubCode(errCode(he.msg))) {
			bs.log.Debug("broker.usage not delivered; retrying next round", "grant", u.GrantID, "err", err)
			return
		}
		bs.mu.Lock()
		if p := bs.pending[u.GrantID]; p != nil {
			// Subtract what was sent; usage collected meanwhile stays.
			p.Tokens -= u.Tokens
			p.Requests -= u.Requests
			p.GPUSeconds -= u.GPUSeconds
			if p.Tokens == 0 && p.Requests == 0 && p.GPUSeconds == 0 {
				delete(bs.pending, u.GrantID)
			}
		}
		bs.mu.Unlock()
		if err != nil {
			bs.log.Warn("broker.usage refused by hub", "grant", u.GrantID, "err", err)
			continue
		}
		if resp.GetDeny() && bs.n.grants != nil {
			bs.n.grants.detach(u.GrantID, "hub denied", false)
		}
	}
}
