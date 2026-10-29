package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker/gpu"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/inventory"
	"github.com/richard-sen27/mycelium/internal/sandbox"
	"github.com/richard-sen27/mycelium/internal/sharing"
)

// DefaultLocalAddr is where the local UI and status endpoint listen.
const DefaultLocalAddr = "127.0.0.1:7777"

// Connection states reported by /api/status.
const (
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateDisconnected = "disconnected"
	StateRevoked      = "revoked"
	// StateExpired: the node certificate has expired; the hub refuses it and
	// the node must be joined again.
	StateExpired = "expired"
)

// runConfig holds the timings and seams of Run. Tests shorten the timings and
// replace the inventory probe.
type runConfig struct {
	heartbeatEvery  time.Duration
	probeEvery      time.Duration
	probeTimeout    time.Duration
	renewCheckEvery time.Duration
	renewRetryEvery time.Duration
	shutdownGrace   time.Duration
	// agentRetryEvery is the retry delay after a failed agent token refresh.
	agentRetryEvery time.Duration
	// usageReportEvery is how often worker usage is reported to the hub.
	usageReportEvery time.Duration
	// workerKillGrace is the time between SIGTERM and SIGKILL for a worker.
	workerKillGrace time.Duration
	// workerKeep is how long an exited worker stays in the listing.
	workerKeep time.Duration
	// workerStartHook, if set, runs just before a worker process starts (tests).
	workerStartHook func(name string)
	probe           func(context.Context) (*mycelv1.Inventory, error)
	// onState, if set, is called with every state change (tests).
	onState func(string)
	// onNode, if set, receives the node once it is loaded (tests).
	onNode func(*node)

	// Phase 4: jobs.
	// egressListen is the egress proxy's listen address (default
	// 127.0.0.1:0; MYCEL_EGRESS_LISTEN overrides it).
	egressListen string
	// jobLogFlushEvery is the longest a log batch waits before it is sent.
	jobLogFlushEvery     time.Duration
	jobLogMaxBuffer      int           // unacknowledged log bytes kept per job
	jobLogDrain          time.Duration // how long logs may take to drain after a job
	jobDoneRetryEvery    time.Duration
	jobDoneRetryFor      time.Duration
	jobKillGrace         time.Duration // TERM to KILL in the sandbox
	jobStatsEvery        time.Duration
	jobKeep              time.Duration // finished job directories are kept this long
	jobsCap              int           // at most this many job directories
	jobJanitorEvery      time.Duration
	backendsRefreshEvery time.Duration
	jobArtifactMax       int64 // job input and output artifact size cap
	localArtifactMax     int64 // trusted-host export and fetch size cap

	// Phase 5: brokers, grants, sharing.
	ollamaUpstream   string               // MYCEL_OLLAMA_UPSTREAM or http://127.0.0.1:11434
	gpuRunner        gpu.Runner           // nil: nvidia-smi
	dockerEngine     http.RoundTripper    // nil: the host Docker Engine
	idleDetector     sharing.IdleDetector // nil: the platform detector
	idleSampleEvery  time.Duration        // idle sampling and local_first evaluation
	brokerUsageEvery time.Duration        // broker.usage batches
	reportPollEvery  time.Duration        // retries of grant.revoked and node.sharing, expiry sweep
	brokerProbeEvery time.Duration        // broker availability refresh
	// now is the clock of grant expiry (tests move it to simulate sleep).
	now func() time.Time
	// grantAttachHook, if set, runs after a broker attached a grant and
	// before the grant is recorded (tests).
	grantAttachHook func(id string)
	// pauseHook, if set, runs before a job sandbox is paused or resumed;
	// an error counts as a failed pause (tests).
	pauseHook func(jobID string, pause bool) error

	// Phase 7: hub discovery and redirects (move.go).
	move moveConfig
}

func defaultRunConfig() runConfig {
	return runConfig{
		heartbeatEvery:   5 * time.Second,
		probeEvery:       60 * time.Second,
		probeTimeout:     15 * time.Second,
		renewCheckEvery:  time.Hour,
		renewRetryEvery:  10 * time.Minute,
		shutdownGrace:    2 * time.Second,
		agentRetryEvery:  10 * time.Second,
		usageReportEvery: 5 * time.Second,
		workerKillGrace:  5 * time.Second,
		workerKeep:       time.Hour,
		probe:            inventory.Probe,

		egressListen:         egressListenDefault(),
		jobLogFlushEvery:     500 * time.Millisecond,
		jobLogMaxBuffer:      4 << 20,
		jobLogDrain:          30 * time.Second,
		jobDoneRetryEvery:    10 * time.Second,
		jobDoneRetryFor:      10 * time.Minute,
		jobKillGrace:         sandbox.DefaultKillGrace,
		jobStatsEvery:        sandbox.DefaultStatsInterval,
		jobKeep:              time.Hour,
		jobsCap:              50,
		jobJanitorEvery:      5 * time.Minute,
		backendsRefreshEvery: 60 * time.Second,
		jobArtifactMax:       maxJobArtifactBytes,
		localArtifactMax:     maxLocalArtifactBytes,

		ollamaUpstream:   ollamaUpstreamDefault(),
		gpuRunner:        defaultGPURunner,
		idleSampleEvery:  30 * time.Second,
		brokerUsageEvery: 10 * time.Second,
		reportPollEvery:  time.Second,
		brokerProbeEvery: 30 * time.Second,
		now:              time.Now,
		move:             defaultMoveConfig(),
	}
}

func egressListenDefault() string {
	if v := os.Getenv(EnvEgressListen); v != "" {
		return v
	}
	return "127.0.0.1:0"
}

// node is the running node role.
type node struct {
	cfg   runConfig
	dir   string
	log   *slog.Logger
	pool  *x509.CertPool
	td    identity.TrustDomain
	start time.Time

	keyPEM []byte // needed to pair a renewed certificate with the key

	mu       sync.Mutex
	st       State
	cert     tls.Certificate
	leaf     *x509.Certificate
	state    string
	lastBeat time.Time
	inv      *mycelv1.Inventory
	invHash  string
	nc       *nats.Conn
	ncGen    uint64 // generation of nc
	nextGen  uint64
	closed   bool // set on teardown: no new connections are installed
	// The last full inventory was sent with this hash, on this connection,
	// after this many reconnects of it. Any difference sends it in full again.
	fullNC         *nats.Conn
	fullReconnects uint64
	sentHash       string // hash of the last full inventory sent
	connected      bool
	expired        bool // the node certificate is past NotAfter

	kick chan struct{} // asks the heartbeat loop for an immediate beat

	local   *localAPI   // local API and agent sessions; set by run
	jobs    *jobManager // jobs in sandboxes; set by run
	sharing *sharingState
	grants  *grantManager
	grantQ  chan func() // AttachGrant and DetachGrant, in order
	freezeQ chan func() // Freeze and Unfreeze, in order

	fzMu      sync.Mutex
	fz        freezeState // network freeze (scope all)
	localAddr string      // bound local API address; set by run before serving

	mv *moveNode // phase 7: redirects and discovery (move.go)

	revokedOnce  sync.Once
	revokedCh    chan struct{}
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	fatalCh      chan error
}

func run(ctx context.Context, opts Options, ready chan<- Ready, cfg runConfig) error {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	addr := opts.LocalAddr
	if addr == "" {
		addr = DefaultLocalAddr
	}
	if err := checkLoopback(addr); err != nil {
		return err
	}

	n, err := loadNode(opts.NodeDir, log, cfg)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("local listener: %w", err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return fmt.Errorf("local listener bound to %s, which is not a loopback address; the local UI is never exposed", ln.Addr())
	}
	n.localAddr = ln.Addr().String()
	if n.local, err = newLocalAPI(n); err != nil {
		_ = ln.Close()
		return err
	}
	n.loadFrozen()
	n.sharing = newSharingState(n)
	n.sharing.sample()
	n.jobs = newJobManager(n)
	n.grants = newGrantManager(n)
	n.grants.b.probeAll(ctx)
	n.grants.restore()
	n.applyStartupFreeze()
	if cfg.onNode != nil {
		cfg.onNode(n)
	}
	srv := &http.Server{Handler: n.httpHandler(), ReadHeaderTimeout: 10 * time.Second}
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.log.Error("local server stopped", "err", err)
		}
	}()
	n.log.Info("local status endpoint listening", "addr", ln.Addr().String())
	if ready != nil {
		select {
		case ready <- Ready{LocalAddr: ln.Addr().String()}:
		case <-ctx.Done():
		}
	}

	ictx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { n.probeLoop(ictx) })
	wg.Go(func() { runCtlQueue(ictx, n.grantQ) })
	wg.Go(func() { runCtlQueue(ictx, n.freezeQ) })
	wg.Go(func() { n.initialConnect(ictx) })
	wg.Go(func() { n.heartbeatLoop(ictx) })
	wg.Go(func() { n.renewLoop(ictx) })
	wg.Go(func() { n.jobs.loop(ictx) })
	wg.Go(func() { n.jobs.egress.loop(ictx) })
	wg.Go(func() { n.sharing.loop(ictx) })
	wg.Go(func() { n.grants.loop(ictx) })
	wg.Go(func() { n.grants.b.usageLoop(ictx) })
	wg.Go(func() { n.grants.b.probeLoop(ictx) })
	wg.Go(func() { n.grants.sweepDocker(ictx) })
	wg.Go(func() { n.moveLoop(ictx) })

	var result error
	revoked := false
	select {
	case <-ctx.Done():
	case <-n.shutdownCh:
		n.log.Info("shutdown requested by hub")
	case <-n.revokedCh:
		revoked = true
		result = ErrRevoked
	case err := <-n.fatalCh:
		result = err
	}

	// Workers end with the daemon (and on revocation); their exits are
	// reported while the bus may still be up.
	n.local.stopAllWorkers(exitKilled)
	// Running jobs are cancelled and reported, best effort.
	n.jobs.shutdown()
	cancel()
	// The last broker usage goes out while the bus may still be up.
	n.grants.b.finalUsage()
	n.grants.close()
	// Agent sessions end with the daemon (and on revocation); the cursors
	// under AgentsDir stay.
	n.local.close()
	n.closeConn()
	wg.Wait()
	if revoked {
		n.forgetIdentity()
	}

	sctx, scancel := context.WithTimeout(context.Background(), cfg.shutdownGrace)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	<-srvDone
	return result
}

// checkLoopback refuses any listen address that is not a loopback address:
// the local UI is never exposed to the network.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("local address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("local address %q is not a loopback address; the local UI is never exposed", addr)
	}
	return nil
}

func loadNode(dir string, log *slog.Logger, cfg runConfig) (*node, error) {
	st, err := ReadState(dir)
	if err != nil {
		return nil, err
	}
	if !identity.ValidNodeName(st.Name) {
		return nil, fmt.Errorf("%s: invalid node name", StateFile)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		return nil, fmt.Errorf("read node key: %w", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		return nil, fmt.Errorf("read node certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, fmt.Errorf("read hub CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s: no certificate", CAFile)
	}
	cert, leaf, err := loadKeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := checkHubURL(st.HubURL); err != nil {
		return nil, fmt.Errorf("%s: %w", StateFile, err)
	}
	td := identity.TrustDomain(st.TrustDomain)
	if td == "" {
		if td, _, err = identity.ParseNodeSPIFFEID(st.SPIFFEID); err != nil {
			return nil, fmt.Errorf("%s: %w", StateFile, err)
		}
	}
	if st.Caps == nil {
		st.Caps = []string{}
	}
	return &node{
		cfg:        cfg,
		dir:        dir,
		log:        log.With("node", st.Name),
		pool:       pool,
		td:         td,
		start:      time.Now(),
		keyPEM:     keyPEM,
		st:         *st,
		cert:       cert,
		leaf:       leaf,
		state:      StateConnecting,
		kick:       make(chan struct{}, 1),
		grantQ:     make(chan func(), ctlQueueSize),
		freezeQ:    make(chan func(), ctlQueueSize),
		revokedCh:  make(chan struct{}),
		shutdownCh: make(chan struct{}),
		fatalCh:    make(chan error, 1),
		mv:         newMoveNode(caPEM),
	}, nil
}

func loadKeyPair(certPEM, keyPEM []byte) (tls.Certificate, *x509.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("node key pair: %w", err)
	}
	leaf := cert.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("node certificate: %w", err)
		}
		cert.Leaf = leaf
	}
	return cert, leaf, nil
}

// ---- state ----

func (n *node) setState(s string) {
	n.mu.Lock()
	n.setStateLocked(s)
	n.mu.Unlock()
}

// setStateLocked changes the state unless the node is already revoked.
func (n *node) setStateLocked(s string) {
	if n.state == StateRevoked || n.state == s {
		return
	}
	n.state = s
	n.connected = s == StateConnected
	n.mv.trackLocked(n.connected)
	if n.cfg.onState != nil {
		n.cfg.onState(s)
	}
}

func (n *node) triggerRevoked() {
	n.revokedOnce.Do(func() {
		n.log.Warn("node revoked by hub; forgetting identity")
		n.setState(StateRevoked)
		// Run closes the connection (without reconnect) as soon as it sees
		// revokedCh; closing here could run inside a NATS callback.
		close(n.revokedCh)
	})
}

func (n *node) triggerShutdown() {
	n.shutdownOnce.Do(func() { close(n.shutdownCh) })
}

func (n *node) fatal(err error) {
	select {
	case n.fatalCh <- err:
	default:
	}
}

func (n *node) wake() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// ---- bus ----

// connectBus dials the bus with the current certificate. It blocks until
// connected or ctx is done.
func (n *node) connectBus(ctx context.Context) (*nats.Conn, uint64, error) {
	n.mu.Lock()
	url := n.st.BusURL
	n.mu.Unlock()
	return n.connectBusTo(ctx, url, false)
}

// connectBusTo dials url. A trial connection (a hub the node is switching
// to) never treats an authorization refusal as revocation before it is
// installed: a hub that refuses this node's certificate must not wipe its
// identity.
func (n *node) connectBusTo(ctx context.Context, url string, trial bool) (*nats.Conn, uint64, error) {
	n.mu.Lock()
	n.nextGen++
	gen := n.nextGen
	cert := n.cert
	name := n.st.Name
	n.mu.Unlock()

	nc, err := bus.Connect(ctx, bus.ClientConfig{
		URL:      url,
		NodeName: name,
		Cert:     cert,
		RootCAs:  n.pool,
		Logger:   n.log,
		OnDisconnect: func(error) {
			n.mu.Lock()
			if n.ncGen == gen && !n.closed {
				n.setStateLocked(StateDisconnected)
			}
			n.mu.Unlock()
		},
		OnReconnect: func() {
			n.mu.Lock()
			if n.ncGen == gen && !n.closed {
				n.setStateLocked(StateConnected)
			}
			n.mu.Unlock()
			n.wake()
		},
		OnAuthRevoked: func() {
			// A stale connection (replaced after renewal, or torn down)
			// must never wipe the identity.
			n.mu.Lock()
			stale := n.closed || gen < n.ncGen || (trial && gen != n.ncGen)
			n.mu.Unlock()
			if !stale {
				n.triggerRevoked()
			}
		},
	})
	return nc, gen, err
}

// install subscribes nc to the ctl subject and makes it the current
// connection, closing the previous one. A connection older than the current
// one, or one arriving after teardown, is closed instead.
func (n *node) install(nc *nats.Conn, gen uint64) error {
	if _, err := nc.Subscribe(bus.NodeCtl(n.name()), func(m *nats.Msg) { n.handleCtl(nc, m) }); err != nil {
		nc.Close()
		return fmt.Errorf("subscribe ctl: %w", err)
	}
	if err := nc.Flush(); err != nil {
		n.log.Warn("bus: flush after subscribe", "err", err)
	}
	n.mu.Lock()
	if n.closed || gen < n.ncGen {
		n.mu.Unlock()
		nc.Close()
		return nil
	}
	old := n.nc
	n.nc, n.ncGen = nc, gen
	if nc.IsConnected() {
		n.setStateLocked(StateConnected)
	}
	n.mu.Unlock()
	if old != nil {
		old.Close()
	}
	n.wake()
	return nil
}

func (n *node) initialConnect(ctx context.Context) {
	nc, gen, err := n.connectBus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// Connect only gives up on an authorization refusal, which already
		// triggered the revoked path, or on a configuration error.
		select {
		case <-n.revokedCh:
		default:
			n.fatal(err)
		}
		return
	}
	if err := n.install(nc, gen); err != nil {
		n.fatal(err)
	}
}

func (n *node) closeConn() {
	n.mu.Lock()
	n.closed = true
	nc := n.nc
	n.mu.Unlock()
	if nc != nil {
		nc.Close()
	}
}

func (n *node) name() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.Name
}

// ---- control ----

func (n *node) handleCtl(nc *nats.Conn, m *nats.Msg) {
	req := &mycelv1.CtlRequest{}
	resp := &mycelv1.CtlResponse{Ok: true}
	after := func() {}
	if err := proto.Unmarshal(m.Data, req); err != nil {
		resp = &mycelv1.CtlResponse{Error: "malformed request"}
	} else {
		switch cmd := req.GetCmd().(type) {
		case *mycelv1.CtlRequest_Ping:
		case *mycelv1.CtlRequest_SetCaps:
			if err := n.setCaps(cmd.SetCaps.GetCapsEnabled().GetNames()); err != nil {
				n.log.Warn("set caps", "err", err)
				resp = &mycelv1.CtlResponse{Error: "could not store caps"}
			}
		case *mycelv1.CtlRequest_Shutdown:
			n.log.Info("hub asked node to shut down", "reason", cmd.Shutdown.GetReason())
			after = n.triggerShutdown
		case *mycelv1.CtlRequest_Agent:
			if e := n.local.agentCtl(cmd.Agent); e != "" {
				resp = &mycelv1.CtlResponse{Error: e}
			}
		case *mycelv1.CtlRequest_CreateSandbox:
			if e := n.jobs.create(cmd.CreateSandbox); e != "" {
				resp = &mycelv1.CtlResponse{Error: e}
			}
		case *mycelv1.CtlRequest_KillSandbox:
			if e := n.jobs.kill(cmd.KillSandbox); e != "" {
				resp = &mycelv1.CtlResponse{Error: e}
			}
		// Grant and freeze controls run on their own queues, in order within
		// each, so a slow grant never delays a freeze (and neither blocks
		// the other controls).
		case *mycelv1.CtlRequest_AttachGrant:
			n.enqueueCtl(n.grantQ, nc, m, func() string { return n.grants.attach(cmd.AttachGrant.GetGrant()) })
			return
		case *mycelv1.CtlRequest_DetachGrant:
			n.enqueueCtl(n.grantQ, nc, m, func() string {
				reason := cmd.DetachGrant.GetReason()
				if reason == "" {
					reason = "detached by hub"
				}
				n.grants.detachFromHub(cmd.DetachGrant.GetGrantId(), reason)
				return ""
			})
			return
		case *mycelv1.CtlRequest_Freeze:
			n.enqueueCtl(n.freezeQ, nc, m, func() string { return n.freeze(cmd.Freeze) })
			return
		case *mycelv1.CtlRequest_Unfreeze:
			n.enqueueCtl(n.freezeQ, nc, m, func() string { return n.unfreeze(cmd.Unfreeze) })
			return
		case *mycelv1.CtlRequest_Sharing:
			if e := n.sharing.setHub(cmd.Sharing, true); e != "" {
				resp = &mycelv1.CtlResponse{Error: e}
			}
		case *mycelv1.CtlRequest_Redirect:
			if e := n.acceptRedirect(cmd.Redirect); e != "" {
				resp = &mycelv1.CtlResponse{Error: e}
			}
		case *mycelv1.CtlRequest_Revoked:
			n.log.Warn("hub revoked node", "reason", cmd.Revoked.GetReason())
			after = n.triggerRevoked
		default:
			resp = &mycelv1.CtlResponse{Error: "unknown command"}
		}
	}
	n.ctlReply(nc, m, resp)
	after()
}

func (n *node) ctlReply(nc *nats.Conn, m *nats.Msg, resp *mycelv1.CtlResponse) {
	b, err := proto.Marshal(resp)
	if err == nil && m.Reply != "" {
		if err := m.Respond(b); err != nil {
			n.log.Warn("ctl reply", "err", err)
		} else {
			_ = nc.FlushTimeout(2 * time.Second)
		}
	}
}

// ctlQueueSize bounds the controls waiting on one queue.
const ctlQueueSize = 256

// enqueueCtl runs work on queue q and replies with its result; a full
// queue is refused at once.
func (n *node) enqueueCtl(q chan func(), nc *nats.Conn, m *nats.Msg, work func() string) {
	job := func() {
		resp := &mycelv1.CtlResponse{Ok: true}
		if e := work(); e != "" {
			resp = &mycelv1.CtlResponse{Error: e}
		}
		n.ctlReply(nc, m, resp)
	}
	select {
	case q <- job:
	default:
		n.ctlReply(nc, m, &mycelv1.CtlResponse{Error: ctlError("unavailable", "node busy")})
	}
}

// runCtlQueue runs the controls of q in order until ctx ends.
func runCtlQueue(ctx context.Context, q chan func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-q:
			job()
		}
	}
}

func (n *node) setCaps(caps []string) error {
	c := append([]string{}, caps...)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.state == StateRevoked {
		return errors.New("node is shutting down or revoked")
	}
	st := n.st
	st.Caps = c
	if err := writeStateFile(n.dir, st); err != nil {
		return err
	}
	n.st = st
	return nil
}

// ---- inventory and heartbeats ----

func (n *node) probeLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.probeEvery)
	defer t.Stop()
	for {
		n.probeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *node) probeOnce(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, n.cfg.probeTimeout)
	defer cancel()
	inv, err := n.cfg.probe(pctx)
	if err != nil {
		if ctx.Err() == nil {
			n.log.Warn("inventory probe failed", "err", err)
		}
		return
	}
	n.mu.Lock()
	// The probe lists what is installed; the hub must see what can run.
	inv.SandboxBackends = n.jobs.availableBackends()
	h := inventory.Hash(inv)
	changed := h != n.invHash
	n.inv, n.invHash = inv, h
	n.mu.Unlock()
	if changed {
		n.wake()
	}
}

// setInventoryBackends replaces inventory.sandbox_backends with the
// available backends and rehashes, so the next beat carries the full
// inventory when the set changed.
func (n *node) setInventoryBackends(list []string) {
	n.mu.Lock()
	if n.inv == nil {
		n.mu.Unlock()
		return // the first probe picks the list up
	}
	inv := proto.Clone(n.inv).(*mycelv1.Inventory)
	inv.SandboxBackends = append([]string{}, list...)
	h := inventory.Hash(inv)
	changed := h != n.invHash
	n.inv, n.invHash = inv, h
	n.mu.Unlock()
	if changed {
		n.wake()
	}
}

func (n *node) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.kick:
		}
		n.beat()
	}
}

func (n *node) beat() {
	n.mu.Lock()
	nc, inv, hash := n.nc, n.inv, n.invHash
	sentHash, fullNC, fullRC := n.sentHash, n.fullNC, n.fullReconnects
	name := n.st.Name
	n.mu.Unlock()
	if nc == nil || !nc.IsConnected() {
		return
	}
	// nats.go counts a reconnect before the connection reports connected,
	// so a beat that sees the connection up also sees its reconnect.
	rc := nc.Stats().Reconnects
	full := inv != nil && (hash != sentHash || nc != fullNC || rc != fullRC)
	now := time.Now()
	hb := &mycelv1.Heartbeat{
		NodeName:      name,
		InventoryHash: hash,
		UptimeSeconds: uint64(max(now.Sub(n.start), 0).Seconds()),
		SentAt:        now.UTC().Format(time.RFC3339Nano),
	}
	if full {
		hb.Inventory = inv
	}
	b, err := proto.Marshal(hb)
	if err != nil {
		n.log.Error("marshal heartbeat", "err", err)
		return
	}
	if err := nc.Publish(bus.NodeTelemetry(name), b); err != nil {
		n.log.Debug("heartbeat not sent", "err", err)
		return
	}
	n.mu.Lock()
	n.lastBeat = now
	if full {
		n.sentHash, n.fullNC, n.fullReconnects = hash, nc, rc
	}
	n.mu.Unlock()
}

// ---- revocation ----

// forgetIdentity removes the node's identity after revocation: node.yaml is
// kept as node.yaml.revoked-<unix ts>, the key and certificate are deleted.
func (n *node) forgetIdentity() {
	// Hold mu so an in-flight SetCaps cannot recreate node.yaml.
	n.mu.Lock()
	defer n.mu.Unlock()
	ts := time.Now().Unix()
	src := filepath.Join(n.dir, StateFile)
	dst := fmt.Sprintf("%s.revoked-%d", src, ts)
	if err := os.Rename(src, dst); err != nil {
		n.log.Error("move node state aside", "err", err)
	}
	for _, f := range []string{KeyFile, CertFile} {
		if err := os.Remove(filepath.Join(n.dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			n.log.Error("remove "+f, "err", err)
		}
	}
}
