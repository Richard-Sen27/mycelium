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
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/inventory"
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
	}
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

	local     *localAPI // local API and agent sessions; set by run
	localAddr string    // bound local API address; set by run before serving

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
	wg.Go(func() { n.initialConnect(ictx) })
	wg.Go(func() { n.heartbeatLoop(ictx) })
	wg.Go(func() { n.renewLoop(ictx) })

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
	cancel()
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
		revokedCh:  make(chan struct{}),
		shutdownCh: make(chan struct{}),
		fatalCh:    make(chan error, 1),
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
	n.nextGen++
	gen := n.nextGen
	cert := n.cert
	name, url := n.st.Name, n.st.BusURL
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
			stale := n.closed || gen < n.ncGen
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
		case *mycelv1.CtlRequest_Revoked:
			n.log.Warn("hub revoked node", "reason", cmd.Revoked.GetReason())
			after = n.triggerRevoked
		default:
			resp = &mycelv1.CtlResponse{Error: "unknown command"}
		}
	}
	b, err := proto.Marshal(resp)
	if err == nil && m.Reply != "" {
		if err := m.Respond(b); err != nil {
			n.log.Warn("ctl reply", "err", err)
		} else {
			_ = nc.FlushTimeout(2 * time.Second)
		}
	}
	after()
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
	h := inventory.Hash(inv)
	n.mu.Lock()
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
