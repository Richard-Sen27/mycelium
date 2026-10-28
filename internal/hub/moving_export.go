package hub

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
	"github.com/richard-sen27/mycelium/internal/tunnel"
)

// Moving the hub, part 2 (moving.go): export and import.
//
// The live export route POST /api/admin/export answers only an admin API
// key on a loopback connection (remote and local address both loopback)
// and streams a tar.gz with the parts that need the running hub: hub.db
// (VACUUM INTO) and the JetStream snapshots (jetstream-snap/<stream>.json
// and .snap). The CLI on the hub machine adds the key and config files
// from the hub directory itself, so the CA key never crosses the API.

const (
	snapDir      = "jetstream-snap"
	snapChunk    = 128 << 10
	snapTimeout  = 10 * time.Minute
	chunkTimeout = 30 * time.Second
)

// exportStreams are the JetStream streams in an export.
var exportStreams = []string{bus.StreamChannels, bus.StreamAudit, bus.StreamJobLogs}

// Required and optional top-level files; directories copied whole. An
// exported relay.url is never restored (Import).
var (
	exportRequired = []string{hubConfigFile, caCertFile, caKeyFile, serverCertFile, serverKeyFile, agentKeyFile}
	exportOptional = []string{vapidFile, tunnel.RelayURLFile}
	exportDirs     = []string{"acme", artifactsSubdir, "jobs"}
)

// ---- tar helpers ----

type tarWriter struct {
	tw *tar.Writer
}

func (t tarWriter) bytes(name string, data []byte) error {
	if err := t.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := t.tw.Write(data)
	return err
}

func (t tarWriter) file(name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := t.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.CopyN(t.tw, f, st.Size())
	return err
}

// dir adds the regular files below src as prefix/...; symlinks and other
// special files are skipped.
func (t tarWriter) dir(prefix, src string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		return t.file(path.Join(prefix, filepath.ToSlash(rel)), p)
	})
}

// ---- live export (running hub) ----

// exportPeerAllowed: the live export is served only to the hub machine
// itself: a connection whose remote and local addresses are both loopback,
// or (for a hub listening on one non-loopback address) whose remote
// address equals the local one. A loopback peer on a non-loopback local
// address (a tunnel or port forward) is refused.
func exportPeerAllowed(remoteAddr string, local net.Addr) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || local == nil {
		return false
	}
	rip := net.ParseIP(host)
	lhost, _, err := net.SplitHostPort(local.String())
	if err != nil || rip == nil {
		return false
	}
	lip := net.ParseIP(lhost)
	if lip == nil {
		return false
	}
	if rip.IsLoopback() || lip.IsLoopback() {
		return rip.IsLoopback() && lip.IsLoopback()
	}
	return rip.Equal(lip) && !rip.IsUnspecified()
}

// errExportPeer is the refusal text; the CLI then tells the user to stop
// the hub and export it from disk.
const errExportPeer = "export is only served to the hub machine itself; stop the hub to export"

func (s *server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	local, _ := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	if !exportPeerAllowed(r.RemoteAddr, local) {
		writeAPIError(w, http.StatusForbidden, errExportPeer)
		return
	}
	u, ok := s.apiKeyAdmin(w, r, "hub.export")
	if !ok {
		return
	}
	if !s.move.exporting.CompareAndSwap(false, true) {
		writeAPIError(w, http.StatusConflict, "an export is already running")
		return
	}
	defer s.move.exporting.Store(false)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(snapTimeout + time.Minute))
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	err := s.writeLiveExport(ctx, w)
	outcome := outcomeOK
	details := map[string]any{"mode": "live"}
	if err != nil {
		outcome = outcomeError
		s.log.Error("export", "err", err)
	}
	s.audit(ctx, u.ID, "hub.export", "hub", outcome, details)
}

// writeLiveExport streams hub.db and the stream snapshots as tar.gz.
func (s *server) writeLiveExport(ctx context.Context, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	t := tarWriter{tw}
	tmp, err := os.MkdirTemp(s.dir, ".export-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dbSnap := filepath.Join(tmp, dbFile)
	if _, err := s.st.db.ExecContext(ctx, `VACUUM INTO ?`, dbSnap); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	if err := t.file(dbFile, dbSnap); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, snapTimeout)
	defer cancel()
	for _, name := range exportStreams {
		snap := filepath.Join(tmp, name+".snap")
		f, err := os.OpenFile(snap, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		meta, err := snapshotStream(sctx, s.nc, name, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("snapshot stream %s: %w", name, err)
		}
		if err := t.bytes(snapDir+"/"+name+".json", meta); err != nil {
			return err
		}
		if err := t.file(snapDir+"/"+name+".snap", snap); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// jsAPIError is the error part of a JetStream API response.
type jsAPIError struct {
	Error *struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"error"`
}

func (e jsAPIError) err() error {
	if e.Error == nil {
		return nil
	}
	return fmt.Errorf("jetstream: %s (%d)", e.Error.Description, e.Error.Code)
}

// snapshotStream writes a JetStream snapshot of stream name to w and
// returns the restore request (its config and state). It speaks the
// JetStream snapshot API: chunks arrive on a deliver subject, each acked
// on its reply subject; an empty message ends the stream.
func snapshotStream(ctx context.Context, nc *nats.Conn, name string, w io.Writer) ([]byte, error) {
	deliver := nc.NewRespInbox()
	sub, err := nc.SubscribeSync(deliver)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	req, _ := json.Marshal(map[string]any{"deliver_subject": deliver, "no_consumers": true, "chunk_size": snapChunk})
	resp, err := nc.RequestWithContext(ctx, "$JS.API.STREAM.SNAPSHOT."+name, req)
	if err != nil {
		return nil, err
	}
	var sr struct {
		jsAPIError
		Config json.RawMessage `json:"config"`
		State  json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(resp.Data, &sr); err != nil {
		return nil, fmt.Errorf("snapshot response: %w", err)
	}
	if err := sr.err(); err != nil {
		return nil, err
	}
	meta, err := json.Marshal(map[string]json.RawMessage{"config": sr.Config, "state": sr.State})
	if err != nil {
		return nil, err
	}
	for {
		cctx, cancel := context.WithTimeout(ctx, chunkTimeout)
		m, err := sub.NextMsgWithContext(cctx)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(m.Data) == 0 {
			if st := m.Header.Get("Status"); st != "" && st != "204" {
				return nil, fmt.Errorf("snapshot failed: %s %s", st, m.Header.Get("Description"))
			}
			return meta, nil
		}
		if _, err := w.Write(m.Data); err != nil {
			return nil, err
		}
		if m.Reply != "" {
			if err := nc.Publish(m.Reply, nil); err != nil {
				return nil, err
			}
		}
	}
}

// restoreStream restores one stream from its snapshot through the
// JetStream restore API.
func restoreStream(ctx context.Context, nc *nats.Conn, name string, meta []byte, r io.Reader) error {
	var cfg struct {
		Config struct {
			Name string `json:"name"`
		} `json:"config"`
	}
	if err := json.Unmarshal(meta, &cfg); err != nil || cfg.Config.Name != name {
		return fmt.Errorf("stream %s: bad snapshot metadata", name)
	}
	resp, err := nc.RequestWithContext(ctx, "$JS.API.STREAM.RESTORE."+name, meta)
	if err != nil {
		return err
	}
	var rr struct {
		jsAPIError
		DeliverSubject string `json:"deliver_subject"`
	}
	if err := json.Unmarshal(resp.Data, &rr); err != nil {
		return fmt.Errorf("restore response: %w", err)
	}
	if err := rr.err(); err != nil {
		return err
	}
	send := func(data []byte) error {
		cctx, cancel := context.WithTimeout(ctx, chunkTimeout)
		defer cancel()
		m, err := nc.RequestWithContext(cctx, rr.DeliverSubject, data)
		if err != nil {
			return err
		}
		if len(m.Data) > 0 {
			var e jsAPIError
			if json.Unmarshal(m.Data, &e) == nil {
				return e.err()
			}
		}
		return nil
	}
	buf := make([]byte, snapChunk)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return send(nil)
}

// ---- Export ----

// ExportOptions configures Export.
type ExportOptions struct {
	HubDir     string
	Passphrase []byte
	// Live is the tar.gz answer of the running hub's POST /api/admin/export;
	// nil when the hub is stopped: Export then takes hub.lock (failing
	// while the hub runs) and reads the database and the jetstream
	// directory from disk.
	Live io.Reader
	// ExcludeArtifacts leaves artifacts/ out (messages keep their
	// references; downloads of old artifacts then fail on the new hub).
	ExcludeArtifacts bool
	Logger           *slog.Logger
}

// Export writes the encrypted export of the hub in HubDir to w, streaming.
func Export(ctx context.Context, opts ExportOptions, w io.Writer) (err error) {
	if len(opts.Passphrase) < MinPassphrase {
		return fmt.Errorf("hub: the passphrase must have at least %d characters", MinPassphrase)
	}
	dir := opts.HubDir
	for _, f := range exportRequired {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return fmt.Errorf("hub: %s holds no hub (%s): %w", dir, f, err)
		}
	}
	var st *store
	if opts.Live == nil {
		unlock, lerr := lockHubDir(dir)
		if lerr != nil {
			return lerr
		}
		defer unlock()
		if st, err = openStore(filepath.Join(dir, dbFile)); err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		// Audited after the seal, with the outcome.
		defer func() {
			details := map[string]any{"mode": "offline", "exclude_artifacts": opts.ExcludeArtifacts}
			outcome := outcomeOK
			if err != nil {
				outcome = outcomeError
				details["error"] = err.Error()
			}
			if aerr := st.appendAudit(context.WithoutCancel(ctx), "cli", "hub.export", "hub", outcome, details); aerr != nil && err == nil {
				err = aerr
			}
		}()
	}
	sw, err := newSealWriter(w, opts.Passphrase)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(sw)
	tw := tar.NewWriter(gz)
	t := tarWriter{tw}
	for _, f := range exportRequired {
		if err := t.file(f, filepath.Join(dir, f)); err != nil {
			return err
		}
	}
	for _, f := range exportOptional {
		if err := t.file(f, filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, d := range exportDirs {
		if d == artifactsSubdir && opts.ExcludeArtifacts {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, d)); err == nil && fi.IsDir() {
			if err := t.dir(d, filepath.Join(dir, d)); err != nil {
				return err
			}
		}
	}
	if opts.Live != nil {
		if err := copyLive(t, opts.Live); err != nil {
			return fmt.Errorf("hub: export from the running hub: %w", err)
		}
	} else if err := exportOffline(ctx, t, dir, st); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return sw.Close()
}

// copyLive copies hub.db and jetstream-snap/* from the running hub's
// answer; anything else in it is refused.
func copyLive(t tarWriter, live io.Reader) error {
	gz, err := gzip.NewReader(live)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	sawDB := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		ok := h.Typeflag == tar.TypeReg && (h.Name == dbFile || (strings.HasPrefix(h.Name, snapDir+"/") && !strings.Contains(h.Name[len(snapDir)+1:], "/")))
		if !ok {
			return fmt.Errorf("unexpected entry %q", h.Name)
		}
		sawDB = sawDB || h.Name == dbFile
		if err := t.tw.WriteHeader(&tar.Header{Name: h.Name, Mode: 0o600, Size: h.Size, ModTime: h.ModTime, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.CopyN(t.tw, tr, h.Size); err != nil {
			return err
		}
	}
	if !sawDB {
		return errors.New("no database in the answer")
	}
	return nil
}

// exportOffline adds a VACUUM INTO snapshot of hub.db and the raw
// jetstream directory of a stopped hub.
func exportOffline(ctx context.Context, t tarWriter, dir string, st *store) error {
	tmp, err := os.MkdirTemp(dir, ".export-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	snap := filepath.Join(tmp, dbFile)
	if _, err := st.db.ExecContext(ctx, `VACUUM INTO ?`, snap); err != nil {
		return fmt.Errorf("hub: snapshot database: %w", err)
	}
	if err := t.file(dbFile, snap); err != nil {
		return err
	}
	js := filepath.Join(dir, jetStreamSubdir)
	if fi, err := os.Stat(js); err == nil && fi.IsDir() {
		return t.dir(jetStreamSubdir, js)
	}
	return nil
}

// ---- Import ----

// ImportOptions configures Import.
type ImportOptions struct {
	HubDir       string    // must be missing or empty
	Source       io.Reader // the export file, read once as a stream
	Passphrase   []byte
	AdvertiseURL string // required
	BusAdvertise string // default tls://<advertise host>:<bus listen port>
	APIListen    string // default: as exported
	BusListen    string // default: as exported
	Logger       *slog.Logger
}

// ImportResult describes an imported hub.
type ImportResult struct {
	TrustDomain   string
	AdvertiseURL  string
	BusAdvertise  string
	CAFingerprint string
	Nodes         int
	Users         int
	OldURL        string // advertise URL of the exported hub
	// RelayDropped: the export held a relay.url, which is never restored
	// (the new hub needs its own relay token).
	RelayDropped bool
}

// checkImportTarget requires dir to be missing or an empty directory, and
// neither dir nor its parent to be a symbolic link.
func checkImportTarget(dir string) (exists bool, err error) {
	for _, p := range []string{filepath.Dir(dir), dir} {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("hub: %s is a symbolic link; import needs a real directory", p)
		}
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("hub: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) > 0 {
		return true, fmt.Errorf("hub: %s already holds a hub or other files; import needs a machine without a hub (an empty or missing directory)", dir)
	}
	return true, nil
}

// Import restores an export into HubDir, which must be missing or empty.
// Everything is restored into a fresh temporary directory next to it,
// which replaces HubDir only when the import has fully succeeded.
func Import(ctx context.Context, opts ImportOptions) (res *ImportResult, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Clean(opts.HubDir)
	if opts.HubDir == "" {
		return nil, errors.New("hub: HubDir is required")
	}
	if opts.Source == nil {
		return nil, errors.New("hub: no export to import")
	}
	adv, err := url.Parse(opts.AdvertiseURL)
	if err != nil || adv.Scheme != "https" || adv.Hostname() == "" {
		return nil, fmt.Errorf("hub: advertise URL must be an https URL with a host, got %q", opts.AdvertiseURL)
	}
	exists, err := checkImportTarget(dir)
	if err != nil {
		return nil, err
	}
	plain, err := newOpenReader(opts.Source, opts.Passphrase)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".import-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp) // everything restored so far, jetstream/ included
		}
	}()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, 0o700); err != nil { //nolint:gosec // a directory needs the x bit
			return nil, err
		}
	}
	relay, err := untarExport(tmp, plain)
	if err != nil {
		return nil, err
	}
	res, err = finishImport(ctx, tmp, adv, opts, log)
	if err != nil {
		return nil, err
	}
	res.RelayDropped = relay
	if exists {
		if err := os.Remove(dir); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, fmt.Errorf("hub: move the imported hub into place: %w", err)
	}
	log.Info("audit", "actor", "cli", "action", "hub.import", "resource", "hub", "outcome", outcomeOK, "from", res.OldURL, "advertise_url", res.AdvertiseURL)
	return res, nil
}

// finishImport rewrites hub.yaml, issues the server certificate, restores
// the streams and audits, all inside dir (the temporary directory).
func finishImport(ctx context.Context, dir string, adv *url.URL, opts ImportOptions, log *slog.Logger) (*ImportResult, error) {
	cfg, err := ReadConfig(dir)
	if err != nil {
		return nil, err
	}
	ca, err := loadHubCA(dir)
	if err != nil {
		return nil, fmt.Errorf("hub: exported CA: %w", err)
	}
	if string(ca.TrustDomain()) != cfg.TrustDomain {
		return nil, errors.New("hub: exported CA does not match hub.yaml")
	}
	oldURL := cfg.AdvertiseURL
	cfg.AdvertiseURL = strings.TrimRight(opts.AdvertiseURL, "/")
	if opts.APIListen != "" {
		cfg.APIListen = opts.APIListen
	}
	if opts.BusListen != "" {
		cfg.BusListen = opts.BusListen
	}
	if _, _, err := net.SplitHostPort(orDefault(cfg.APIListen, defaultAPIListen)); err != nil {
		return nil, fmt.Errorf("hub: api listen %q: %w", cfg.APIListen, err)
	}
	_, busPort, err := net.SplitHostPort(orDefault(cfg.BusListen, defaultBusListen))
	if err != nil {
		return nil, fmt.Errorf("hub: bus listen %q: %w", cfg.BusListen, err)
	}
	advHost := strings.ToLower(adv.Hostname())
	cfg.BusAdvertise = opts.BusAdvertise
	if cfg.BusAdvertise == "" {
		cfg.BusAdvertise = "tls://" + net.JoinHostPort(advHost, busPort)
	}
	cfg.MovedTo = ""
	if cfg.UI.ExternalURL == oldURL {
		cfg.UI.ExternalURL = ""
	}

	// A server certificate for the new host.
	hostname, _ := os.Hostname()
	hosts := []string{advHost, "localhost", "127.0.0.1", "::1"}
	if bh, _, err := net.SplitHostPort(strings.TrimPrefix(cfg.BusAdvertise, "tls://")); err == nil && bh != "" && bh != advHost {
		hosts = append(hosts, strings.ToLower(bh))
	}
	if h := strings.ToLower(hostname); h != "" && h != advHost {
		hosts = append(hosts, h)
	}
	srvCert, srvKey, err := ca.IssueServerCert(hosts, identity.ServerCertLifetime)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, serverCertFile), srvCert); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, serverKeyFile), srvKey); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, jetStreamSubdir), 0o700); err != nil {
		return nil, err
	}
	if err := restoreSnapshots(ctx, dir, ca, srvCert, srvKey, log); err != nil {
		return nil, err
	}
	if err := writeConfig(dir, cfg); err != nil {
		return nil, err
	}

	st, err := openStore(filepath.Join(dir, dbFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	nodes, err := st.listNodes(ctx)
	if err != nil {
		return nil, err
	}
	users, err := st.listUsers(ctx)
	if err != nil {
		return nil, err
	}
	details := map[string]any{"from": oldURL, "advertise_url": cfg.AdvertiseURL, "nodes": len(nodes)}
	if err := st.appendAudit(ctx, "cli", "hub.import", "hub", outcomeOK, details); err != nil {
		return nil, err
	}
	return &ImportResult{
		TrustDomain: cfg.TrustDomain, AdvertiseURL: cfg.AdvertiseURL, BusAdvertise: cfg.BusAdvertise,
		CAFingerprint: ca.Fingerprint(), Nodes: len(nodes), Users: len(users), OldURL: oldURL,
	}, nil
}

// importAllowed reports whether an archive entry may be restored.
func importAllowed(name string) bool {
	top, rest, nested := strings.Cut(name, "/")
	if !nested {
		for _, f := range append(append([]string{dbFile}, exportRequired...), exportOptional...) {
			if name == f {
				return true
			}
		}
		return false
	}
	if rest == "" {
		return false
	}
	switch top {
	case snapDir:
		return !strings.Contains(rest, "/")
	case jetStreamSubdir:
		return true
	}
	for _, d := range exportDirs {
		if top == d {
			return true
		}
	}
	return false
}

// untarExport writes the archive's files below dir (a fresh directory).
// Only known names, regular files and paths that stay inside dir are
// accepted, and no directory on the way may be a symbolic link. relay.url
// is skipped; relay reports whether it was there.
func untarExport(dir string, plain io.Reader) (relay bool, err error) {
	gz, err := gzip.NewReader(plain)
	if err != nil {
		if errors.Is(err, ErrExportDecrypt) {
			return false, err
		}
		return false, fmt.Errorf("hub: export archive: %w", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, ErrExportDecrypt) {
			return relay, err
		}
		if err != nil {
			return relay, fmt.Errorf("hub: export archive: %w", err)
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || name != path.Clean(name) || path.IsAbs(name) || strings.HasPrefix(name, "../") ||
			name == ".." || strings.Contains(name, "\\") || !importAllowed(name) {
			return relay, fmt.Errorf("hub: export archive: refusing entry %q", name)
		}
		if name == tunnel.RelayURLFile {
			relay = true
			continue
		}
		seen[strings.SplitN(name, "/", 2)[0]] = true
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := mkdirNoSymlink(dir, filepath.Dir(dst)); err != nil {
			return relay, err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return relay, err
		}
		_, err = io.CopyN(f, tr, h.Size)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			if errors.Is(err, ErrExportDecrypt) {
				return relay, err
			}
			return relay, fmt.Errorf("hub: export archive: %w", err)
		}
	}
	for _, f := range append([]string{dbFile}, exportRequired...) {
		if !seen[f] {
			return relay, fmt.Errorf("hub: export archive lacks %s", f)
		}
	}
	return relay, nil
}

// mkdirNoSymlink creates sub (below root) and its parents, refusing any
// component that exists as a symbolic link.
func mkdirNoSymlink(root, sub string) error {
	rel, err := filepath.Rel(root, sub)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("hub: export archive: path outside the hub directory")
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
			return fmt.Errorf("hub: export archive: %s is not a plain directory", cur)
		}
	}
	return nil
}

type noRevocation struct{}

func (noRevocation) IsRevoked(string, string) bool { return false }

// restoreSnapshots restores jetstream-snap/* into the jetstream directory
// through a temporary loopback bus, then removes the snapshots.
func restoreSnapshots(ctx context.Context, dir string, ca *identity.CA, certPEM, keyPEM []byte, log *slog.Logger) error {
	snaps := filepath.Join(dir, snapDir)
	if _, err := os.Stat(snaps); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	bs, err := bus.StartServer(bus.ServerConfig{
		Host: "127.0.0.1", Port: 0, TLSCert: cert, ClientCAs: ca.Pool(), TrustDomain: ca.TrustDomain(),
		StoreDir: filepath.Join(dir, jetStreamSubdir), Revocation: noRevocation{}, Logger: log,
	})
	if err != nil {
		return fmt.Errorf("hub: restore streams: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bs.Shutdown(sctx)
	}()
	nc, err := bs.HubConn()
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, snapTimeout)
	defer cancel()
	for _, name := range exportStreams {
		meta, err := os.ReadFile(filepath.Join(snaps, name+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(snaps, name+".snap"))
		if err != nil {
			return err
		}
		err = restoreStream(rctx, nc, name, meta, f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("hub: restore stream %s: %w", name, err)
		}
	}
	return os.RemoveAll(snaps)
}
