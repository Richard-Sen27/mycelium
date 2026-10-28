package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// Roles as stored in the users table.
const (
	roleAdmin  = "admin"
	roleMember = "member"
)

// knownCaps is the set of capabilities a node may offer in phase 1.
var knownCaps = []string{"docker", "ollama", "gpu", "sandbox"}

const (
	defaultAPIListen = ":7777"
	defaultBusListen = ":7778"
)

func initHub(ctx context.Context, opts InitOptions) (res *InitResult, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.HubDir == "" || opts.NodeDir == "" {
		return nil, errors.New("hub: HubDir and NodeDir are required")
	}
	for _, f := range []string{hubConfigFile, dbFile, caKeyFile} {
		if _, err := os.Stat(filepath.Join(opts.HubDir, f)); err == nil {
			return nil, fmt.Errorf("hub: %s already holds a hub (%s exists)", opts.HubDir, f)
		}
	}
	if daemon.HasState(opts.NodeDir) {
		return nil, fmt.Errorf("hub: %s already holds a node; this machine has joined another hub", opts.NodeDir)
	}

	// ---- options and defaults ----
	adv, err := url.Parse(opts.AdvertiseURL)
	if err != nil || adv.Scheme != "https" || adv.Hostname() == "" {
		return nil, fmt.Errorf("hub: advertise URL must be an https URL with a host, got %q", opts.AdvertiseURL)
	}
	advHost := strings.ToLower(adv.Hostname())
	td := identity.TrustDomain(opts.TrustDomain)
	if td == "" {
		td = identity.TrustDomain(advHost)
	}
	if !identity.ValidTrustDomain(td) {
		return nil, fmt.Errorf("hub: invalid trust domain %q (use lowercase letters, digits, '-', '.', '_'; set it explicitly)", td)
	}
	apiListen := orDefault(opts.APIListen, defaultAPIListen)
	busListen := orDefault(opts.BusListen, defaultBusListen)
	if _, _, err := net.SplitHostPort(apiListen); err != nil {
		return nil, fmt.Errorf("hub: api listen %q: %w", apiListen, err)
	}
	_, busPort, err := net.SplitHostPort(busListen)
	if err != nil {
		return nil, fmt.Errorf("hub: bus listen %q: %w", busListen, err)
	}
	busAdvertise := opts.BusAdvertise
	if busAdvertise == "" {
		busAdvertise = "tls://" + net.JoinHostPort(advHost, busPort)
	}
	adminName := orDefault(opts.AdminName, "admin")
	if !validUserName(adminName) {
		return nil, fmt.Errorf("hub: invalid admin name %q", adminName)
	}
	hostname, _ := os.Hostname()
	localNode := opts.LocalNode
	if localNode == "" {
		localNode = sanitizeNodeName(hostname)
	}
	if !identity.ValidNodeName(localNode) {
		return nil, fmt.Errorf("hub: invalid local node name %q", localNode)
	}

	// ---- files; removed again if anything below fails ----
	if err := os.MkdirAll(opts.HubDir, 0o700); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(opts.HubDir, 0o700); err != nil { //nolint:gosec // a directory needs the x bit
			return nil, err
		}
	}
	var created []string
	var st *store
	nodeWritten := false
	defer func() {
		if err == nil {
			return
		}
		if st != nil {
			_ = st.Close()
		}
		for _, p := range created {
			_ = os.Remove(p)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(filepath.Join(opts.HubDir, dbFile+suffix))
		}
		if nodeWritten {
			for _, f := range []string{daemon.StateFile, daemon.KeyFile, daemon.CertFile, daemon.CAFile} {
				_ = os.Remove(filepath.Join(opts.NodeDir, f))
			}
		}
	}()
	write := func(name string, data []byte, mode os.FileMode) error {
		p := filepath.Join(opts.HubDir, name)
		// O_EXCL: never write key material through a pre-existing file or link.
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		created = append(created, p)
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}

	ca, err := identity.NewCA(td)
	if err != nil {
		return nil, err
	}
	hosts := []string{advHost, "localhost", "127.0.0.1", "::1"}
	if h := strings.ToLower(hostname); h != "" && h != advHost {
		hosts = append(hosts, h)
	}
	srvCert, srvKey, err := ca.IssueServerCert(hosts, identity.ServerCertLifetime)
	if err != nil {
		return nil, err
	}
	agentKey, err := identity.NewAgentTokenKey()
	if err != nil {
		return nil, err
	}
	if err = write(caCertFile, ca.CertPEM(), 0o644); err != nil {
		return nil, err
	}
	if err = write(caKeyFile, ca.KeyPEM(), 0o600); err != nil {
		return nil, err
	}
	if err = write(serverCertFile, srvCert, 0o644); err != nil {
		return nil, err
	}
	if err = write(serverKeyFile, srvKey, 0o600); err != nil {
		return nil, err
	}
	if err = write(agentKeyFile, agentKey, 0o600); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(opts.HubDir, jetStreamSubdir), 0o700); err != nil {
		return nil, err
	}

	// ---- database, admin ----
	st, err = openStore(filepath.Join(opts.HubDir, dbFile))
	if err != nil {
		return nil, err
	}
	admin, _, adminKey, err := st.createUser(ctx, adminName, roleAdmin)
	if err != nil {
		return nil, err
	}
	if err = ensureSystemChannels(ctx, st, admin.ID); err != nil {
		return nil, err
	}
	if err = syncProfiles(ctx, st, false, log); err != nil {
		return nil, err
	}

	// ---- local node, signed in-process ----
	key, keyPEM, err := identity.GenerateKey()
	if err != nil {
		return nil, err
	}
	csr, err := identity.NewCSR(key, localNode)
	if err != nil {
		return nil, err
	}
	certPEM, serial, err := ca.SignNodeCSR(csr, localNode, identity.NodeCertLifetime)
	if err != nil {
		return nil, err
	}
	notAfter, err := certNotAfter(certPEM)
	if err != nil {
		return nil, err
	}
	spiffeID := identity.NodeSPIFFEID(td, localNode)
	caps := append([]string(nil), knownCaps...)
	if err = st.insertNode(ctx, &nodeRow{
		Name: localNode, SPIFFEID: spiffeID, OwnerID: admin.ID,
		CapsCeiling: caps, CapsEnabled: caps,
		CertSerial: serial, CertNotAfter: formatTime(notAfter), CreatedAt: nowString(),
	}); err != nil {
		return nil, err
	}
	nodeWritten = true
	if err = daemon.WriteState(opts.NodeDir, daemon.State{
		Name: localNode, SPIFFEID: spiffeID, TrustDomain: string(td),
		HubURL: opts.AdvertiseURL, BusURL: busAdvertise, OwnerID: admin.ID, Caps: caps,
	}, keyPEM, certPEM, ca.CertPEM()); err != nil {
		if errors.Is(err, daemon.ErrNodeExists) {
			nodeWritten = false
		}
		return nil, err
	}

	cfg := &Config{
		TrustDomain: string(td), AdvertiseURL: opts.AdvertiseURL, BusAdvertise: busAdvertise,
		APIListen: apiListen, BusListen: busListen, CreatedAt: nowString(),
	}
	if err = writeConfig(opts.HubDir, cfg); err != nil {
		return nil, err
	}
	created = append(created, filepath.Join(opts.HubDir, hubConfigFile))

	details := map[string]any{"trust_domain": string(td), "advertise_url": opts.AdvertiseURL, "admin": adminName, "local_node": localNode}
	if err = st.appendAudit(ctx, "hub", "hub.init", "hub", outcomeOK, details); err != nil {
		return nil, err
	}
	log.Info("audit", "actor", "hub", "action", "hub.init", "resource", "hub", "outcome", outcomeOK,
		"trust_domain", string(td), "local_node", localNode)
	if err = st.Close(); err != nil {
		st = nil
		return nil, err
	}
	st = nil

	return &InitResult{
		TrustDomain:   string(td),
		AdvertiseURL:  opts.AdvertiseURL,
		CAFingerprint: ca.Fingerprint(),
		CACertPEM:     ca.CertPEM(),
		AdminUserID:   admin.ID,
		AdminAPIKey:   adminKey,
		LocalNodeName: localNode,
	}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sanitizeNodeName turns a hostname into a valid node name: lowercase, runs
// of invalid characters replaced by "-", trimmed, at most 63 chars.
func sanitizeNodeName(h string) string {
	h = strings.ToLower(h)
	// Drop a domain suffix such as ".local".
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	var b strings.Builder
	dash := false
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	if out == "" {
		out = "hub"
	}
	return out
}

// validUserName applies the node name rule to user names.
func validUserName(name string) bool { return identity.ValidNodeName(name) }

func certNotAfter(certPEM []byte) (time.Time, error) {
	c, err := parseCertPEM(certPEM)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}
