package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"

	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// newPolicy builds the policy of a running hub; tests may replace it.
var newPolicy = policy.Default

// maxRequestBytes bounds Connect request bodies.
const maxRequestBytes = 1 << 20

// server is the running hub role.
type server struct {
	cfg    *Config
	td     identity.TrustDomain
	ca     *identity.CA
	caPEM  []byte
	signer *identity.AgentTokenSigner
	st     *store
	bus    *bus.Server
	nc     *nats.Conn
	policy policy.Policy
	log    *slog.Logger

	enrolLimit *rateLimiter
	beats      *beatState
}

func runHub(ctx context.Context, opts Options, ready chan<- Ready) (err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "hub")
	dir := opts.HubDir
	cfg, err := ReadConfig(dir)
	if err != nil {
		return fmt.Errorf("hub: read config: %w", err)
	}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	caPEM, err := read(caCertFile)
	if err != nil {
		return err
	}
	caKey, err := read(caKeyFile)
	if err != nil {
		return err
	}
	ca, err := identity.LoadCA(caPEM, caKey)
	if err != nil {
		return err
	}
	if string(ca.TrustDomain()) != cfg.TrustDomain {
		return fmt.Errorf("hub: CA trust domain %q does not match hub.yaml %q", ca.TrustDomain(), cfg.TrustDomain)
	}
	srvCertPEM, err := read(serverCertFile)
	if err != nil {
		return err
	}
	srvKeyPEM, err := read(serverKeyFile)
	if err != nil {
		return err
	}
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		return fmt.Errorf("hub: server certificate: %w", err)
	}
	agentKey, err := read(agentKeyFile)
	if err != nil {
		return err
	}
	signer, err := identity.NewAgentTokenSigner(agentKey)
	if err != nil {
		return err
	}
	st, err := openStore(filepath.Join(dir, dbFile))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	s := &server{
		cfg: cfg, td: ca.TrustDomain(), ca: ca, caPEM: ca.CertPEM(), signer: signer,
		st: st, policy: newPolicy(), log: log,
		enrolLimit: newRateLimiter(enrolRatePerMinute, enrolBurst),
		beats:      newBeatState(),
	}

	// ---- bus ----
	busListen := orDefault(opts.BusListen, orDefault(cfg.BusListen, defaultBusListen))
	busHost, busPortStr, err := net.SplitHostPort(busListen)
	if err != nil {
		return fmt.Errorf("hub: bus listen %q: %w", busListen, err)
	}
	busPort, err := strconv.Atoi(busPortStr)
	if err != nil {
		return fmt.Errorf("hub: bus listen port %q: %w", busPortStr, err)
	}
	bs, err := bus.StartServer(bus.ServerConfig{
		Host: busHost, Port: busPort, TLSCert: srvCert, ClientCAs: ca.Pool(), TrustDomain: s.td,
		StoreDir: filepath.Join(dir, jetStreamSubdir), Revocation: st, Logger: log,
	})
	if err != nil {
		return err
	}
	s.bus = bs
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := bs.Shutdown(sctx); serr != nil {
			log.Warn("bus shutdown", "err", serr)
		}
	}()
	nc, err := bs.HubConn()
	if err != nil {
		return err
	}
	s.nc = nc
	subs, err := s.startRegistry()
	if err != nil {
		return err
	}
	defer func() {
		// Let in-flight node messages finish before the bus and store close.
		if derr := nc.Drain(); derr != nil {
			for _, sub := range subs {
				_ = sub.Unsubscribe()
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for !nc.IsClosed() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		s.flushBeats()
	}()

	// ---- API ----
	apiListen := orDefault(opts.APIListen, orDefault(cfg.APIListen, defaultAPIListen))
	ln, err := net.Listen("tcp", apiListen)
	if err != nil {
		return fmt.Errorf("hub: api listen: %w", err)
	}
	httpSrv := &http.Server{
		Handler: s.authMiddleware(s.mux()),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{srvCert},
			ClientCAs:    ca.Pool(),
			ClientAuth:   tls.VerifyClientCertIfGiven,
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ServeTLS(ln, "", "") }()

	apiAddr := ln.Addr().String()
	busAddr := net.JoinHostPort(localHost(busHost), strconv.Itoa(bs.Port()))
	log.Info("hub running", "api", apiAddr, "bus", busAddr, "trust_domain", s.td)
	if ready != nil {
		r := Ready{APIAddr: apiAddr, BusAddr: busAddr, HubURL: "https://" + localAddr(apiAddr)}
		select {
		case ready <- r:
		case <-ctx.Done():
		}
	}

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		err = fmt.Errorf("hub: api server: %w", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if serr := httpSrv.Shutdown(sctx); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		log.Warn("api shutdown", "err", serr)
	}
	return err
}

// localHost maps an unspecified listen host to the loopback address.
func localHost(h string) string {
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "127.0.0.1"
	}
	return h
}

func localAddr(addr string) string {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort(localHost(h), p)
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithReadMaxBytes(maxRequestBytes)}
	mux.Handle(mycelv1connect.NewTokenServiceHandler(&tokenService{s}, opts...))
	mux.Handle(mycelv1connect.NewNodeServiceHandler(&nodeService{s}, opts...))
	mux.Handle(mycelv1connect.NewUserServiceHandler(&userService{s}, opts...))
	mux.Handle(mycelv1connect.NewAgentServiceHandler(&agentService{s}, opts...))
	mux.Handle(mycelv1connect.NewEnrolServiceHandler(&enrolService{s}, opts...))
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(s.caPEM)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		title := html.EscapeString("Mycelium hub " + string(s.td))
		_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body><h1>%s</h1></body></html>\n", title, title)
	})
	return mux
}

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("hub: no certificate PEM")
	}
	return x509.ParseCertificate(blk.Bytes)
}
