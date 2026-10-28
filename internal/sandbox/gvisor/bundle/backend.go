package bundle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Mode says where the sandbox host is relative to the daemon.
type Mode int

// Modes.
const (
	// ModeLinux: the daemon runs on the Linux host that runs runsc; the
	// egress proxy is reached at the host side of each sandbox's veth pair.
	ModeLinux Mode = iota
	// ModeWSL: runsc runs inside the WSL distro; the egress proxy runs on
	// Windows and is reached at the distro's default gateway.
	ModeWSL
)

// DefaultStateDir is the state directory on the sandbox host.
const DefaultStateDir = "/var/lib/mycel/sbx"

// DefaultCgroupParent is the cgroup under which each sandbox gets
// <parent>/<container id>.
const DefaultCgroupParent = "/mycel"

// Config wires a Backend.
type Config struct {
	Name     string // sandbox.BackendGVisor or sandbox.BackendWSL2GVisor
	Mode     Mode
	Runner   Runner // ExecRunner{} when nil
	Host     Host   // LocalHost{} when nil (WSLHost for ModeWSL)
	StateDir string // DefaultStateDir when empty; a path on the sandbox host
	Runsc    Runsc  // Bin only; Root, Network and Overlay are set per call
	Docker   Docker
	Pool     netip.Prefix // DefaultPool when invalid
	// Firewall forces the rule tool; detected (nft first) when empty.
	Firewall Firewall
	// AllowDNS lets sandboxes query DNS at the proxy host's address. Off by
	// default: the proxy resolves names, and resolv.conf has no nameserver.
	AllowDNS bool
	// User is the workload uid:gid (DefaultUser when nil).
	User *User
	// CgroupParent is DefaultCgroupParent when empty.
	CgroupParent string
	// Init overrides the container init argv (DefaultInit).
	Init []string
	Now  func() time.Time
}

// Backend implements sandbox.Backend on runsc.
type Backend struct {
	cfg   Config
	slots *SlotAllocator

	gcOnce sync.Once

	mu        sync.Mutex
	digestMu  map[string]*sync.Mutex
	images    map[string]string // image ref -> extracted rootfs dir
	firewall  Firewall
	live      map[string]bool // container ids of sandboxes not yet destroyed
	hostRules string          // key of the installed host-wide rules
}

var _ sandbox.Backend = (*Backend)(nil)

// New returns a Backend.
func New(cfg Config) *Backend {
	if cfg.Runner == nil {
		cfg.Runner = ExecRunner{}
	}
	if cfg.Host == nil {
		if cfg.Mode == ModeWSL {
			cfg.Host = WSLHost{}
		} else {
			cfg.Host = LocalHost{}
		}
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	cfg.StateDir = strings.TrimRight(cfg.StateDir, "/")
	if !cfg.Pool.IsValid() {
		cfg.Pool = DefaultPool
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.User == nil {
		u := DefaultUser
		cfg.User = &u
	}
	if cfg.CgroupParent == "" {
		cfg.CgroupParent = DefaultCgroupParent
	}
	cfg.Runsc.Root = cfg.StateDir + "/runsc"
	return &Backend{
		cfg:      cfg,
		slots:    NewSlotAllocator(cfg.Pool),
		digestMu: map[string]*sync.Mutex{},
		images:   map[string]string{},
		firewall: cfg.Firewall,
		live:     map[string]bool{},
	}
}

// Name implements sandbox.Backend.
func (b *Backend) Name() string { return b.cfg.Name }

// Capabilities implements sandbox.Backend. Disk is measured, not capped.
// Nested Docker is not available inside these sandboxes in v1.
func (b *Backend) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Egress:        true,
		NestedDocker:  false,
		ProcessLimits: true,
		MemoryLimit:   true,
		CPULimit:      true,
		Isolation:     "gvisor",
	}
}

// wrap checks and wraps argv for the sandbox host.
func (b *Backend) wrap(argv []string) ([]string, error) {
	if err := b.cfg.Host.Check(argv); err != nil {
		return nil, err
	}
	return b.cfg.Host.Wrap(argv), nil
}

// run runs argv on the sandbox host.
func (b *Backend) run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) error {
	w, err := b.wrap(argv)
	if err != nil {
		return err
	}
	return b.cfg.Runner.Run(ctx, Cmd{Argv: w, Stdin: stdin, Stdout: stdout})
}

func (b *Backend) out(ctx context.Context, argv []string) ([]byte, error) {
	w, err := b.wrap(argv)
	if err != nil {
		return nil, err
	}
	return output(ctx, b.cfg.Runner, w)
}

// Available implements sandbox.Backend.
func (b *Backend) Available(ctx context.Context) (bool, string) {
	if b.cfg.Mode == ModeWSL {
		h, _ := b.cfg.Host.(WSLHost)
		if err := b.cfg.Runner.Run(ctx, Cmd{Argv: h.WSLStatusArgv()}); err != nil {
			return false, "WSL is not available (wsl.exe --status failed): " + err.Error()
		}
		list, err := output(ctx, b.cfg.Runner, h.WSLListArgv())
		listed, err := WSLDistroListed(list, err, h.distro())
		if err != nil {
			return false, "cannot list WSL distros: " + err.Error()
		}
		if !listed {
			return false, fmt.Sprintf("the sandbox WSL distro %s is not installed (run `mycel sandbox setup`)", h.distro())
		}
	}
	where := ""
	if b.cfg.Mode == ModeWSL {
		where = " in the WSL distro"
	}
	if _, err := b.out(ctx, b.cfg.Runsc.Version()); err != nil {
		return false, "runsc not found" + where + ": " + err.Error()
	}
	if err := b.run(ctx, ToolVersionArgv("ip"), nil, nil); err != nil {
		return false, "ip (iproute2) not found" + where + ": " + err.Error()
	}
	if _, err := b.detectFirewall(ctx); err != nil {
		return false, err.Error() + where
	}
	if b.cfg.Mode == ModeLinux {
		if err := b.cfg.Runner.Run(ctx, Cmd{Argv: b.cfg.Docker.Version()}); err != nil {
			return false, "docker client not found (needed to export image root filesystems): " + err.Error()
		}
	}
	return true, ""
}

func hasLine(s, want string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(strings.TrimPrefix(l, "\uFEFF")) == want {
			return true
		}
	}
	return false
}

func (b *Backend) detectFirewall(ctx context.Context) (Firewall, error) {
	b.mu.Lock()
	fw := b.firewall
	b.mu.Unlock()
	if fw != "" {
		return fw, nil
	}
	switch {
	case b.run(ctx, ToolVersionArgv("nft"), nil, nil) == nil:
		fw = FirewallNFT
	case b.run(ctx, ToolVersionArgv("iptables"), nil, nil) == nil:
		fw = FirewallIPTables
	default:
		return "", errors.New("neither nft nor iptables found")
	}
	b.mu.Lock()
	b.firewall = fw
	b.mu.Unlock()
	return fw, nil
}

var digestRe = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

func (b *Backend) lockDigest(hex string) func() {
	b.mu.Lock()
	m, ok := b.digestMu[hex]
	if !ok {
		m = &sync.Mutex{}
		b.digestMu[hex] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ImageDir is the extracted, shared read-only rootfs for a digest
// ("sha256:…"): the lower layer of every sandbox's root overlay.
func (b *Backend) ImageDir(digest string) (string, error) {
	m := digestRe.FindStringSubmatch(strings.TrimSpace(digest))
	if m == nil {
		return "", fmt.Errorf("bundle: unexpected image id %q", strings.TrimSpace(digest))
	}
	return b.cfg.StateDir + "/images/" + m[1], nil
}

func randSuffix() string {
	var r [6]byte
	_, _ = rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

// Prepare implements sandbox.Backend: it makes sure the image's root
// filesystem exists, extracted once per digest under <state>/images/<hex>.
// It exports the image with the Docker client (`docker create` + `docker
// export`), filters the stream (FilterRootfs) and extracts it into a unique
// <hex>.part.<rand> directory that is renamed into place when complete.
func (b *Backend) Prepare(ctx context.Context, image string, progress func(string)) error {
	if image == "" || strings.HasPrefix(image, "-") {
		return fmt.Errorf("bundle: bad image %q", image)
	}
	if progress == nil {
		progress = func(string) {}
	}
	d := b.cfg.Docker
	r := b.cfg.Runner
	id, err := output(ctx, r, d.ImageID(image))
	if err != nil {
		progress("pulling " + image)
		if err := r.Run(ctx, Cmd{Argv: d.Pull(image)}); err != nil {
			return fmt.Errorf("bundle: docker pull %s: %w", image, err)
		}
		if id, err = output(ctx, r, d.ImageID(image)); err != nil {
			return fmt.Errorf("bundle: docker image inspect %s: %w", image, err)
		}
	}
	dir, err := b.ImageDir(string(id))
	if err != nil {
		return err
	}
	defer b.lockDigest(dir)()
	if b.run(ctx, TestDirArgv(dir), nil, nil) == nil {
		b.setImage(image, dir)
		return nil
	}
	progress("exporting " + image)
	part := dir + ".part." + randSuffix()
	if err := b.run(ctx, MkdirModeArgv(0o700, b.cfg.StateDir+"/images"), nil, nil); err != nil {
		return err
	}
	if err := b.run(ctx, MkdirModeArgv(0o755, part), nil, nil); err != nil {
		return err
	}
	cleanup := func() { _ = b.run(context.WithoutCancel(ctx), RemoveAllArgv(part), nil, nil) }
	cidOut, err := output(ctx, r, d.Create(image))
	if err != nil {
		cleanup()
		return fmt.Errorf("bundle: docker create %s: %w", image, err)
	}
	cid := strings.TrimSpace(string(cidOut))
	if cid == "" || strings.HasPrefix(cid, "-") || strings.ContainsAny(cid, " \n") {
		cleanup()
		return fmt.Errorf("bundle: docker create printed %q", cid)
	}
	defer func() { _ = r.Run(context.WithoutCancel(ctx), Cmd{Argv: d.Remove(cid)}) }()
	extract, err := b.wrap(ExtractArgv(part))
	if err != nil {
		cleanup()
		return err
	}
	var st FilterStats
	if err := b.stream(ctx, d.Export(cid), extract, &st); err != nil {
		cleanup()
		return fmt.Errorf("bundle: export %s: %w", image, err)
	}
	if err := b.run(ctx, MoveArgv(part, dir), nil, nil); err != nil {
		cleanup()
		return err
	}
	b.setImage(image, dir)
	progress(fmt.Sprintf("ready %s (%d entries, %d dropped, %d setuid/setgid stripped)", image, st.Kept, st.Dropped, st.Stripped))
	return nil
}

// stream pipes the stdout of src through FilterRootfs into the stdin of dst.
func (b *Backend) stream(ctx context.Context, src, dst []string, st *FilterStats) error {
	pr, pw := io.Pipe() // src -> filter
	fr, fw := io.Pipe() // filter -> dst
	h, err := b.cfg.Runner.Start(ctx, Cmd{Argv: dst, Stdin: fr})
	if err != nil {
		return err
	}
	dstErr := make(chan error, 1)
	go func() {
		err := h.Wait()
		_ = fr.CloseWithError(errors.New("bundle: extractor exited"))
		dstErr <- err
	}()
	filterErr := make(chan error, 1)
	go func() {
		s, err := FilterRootfs(pr, fw)
		*st = s
		_ = fw.CloseWithError(err)
		_ = pr.CloseWithError(errors.New("bundle: filter stopped"))
		filterErr <- err
	}()
	srcErr := b.cfg.Runner.Run(ctx, Cmd{Argv: src, Stdout: pw})
	_ = pw.CloseWithError(srcErr)
	ferr := <-filterErr
	werr := <-dstErr
	if srcErr != nil {
		return srcErr
	}
	if ferr != nil {
		return ferr
	}
	return werr
}

func (b *Backend) setImage(image, dir string) {
	b.mu.Lock()
	b.images[image] = dir
	b.mu.Unlock()
}

func (b *Backend) imagePath(image string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.images[image]
	return p, ok
}

// sandboxIDRe is the only accepted sandbox id: "sbx:" and a ULID (Crockford
// base32, either case).
var sandboxIDRe = regexp.MustCompile(`^sbx:[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)

// ErrSandboxID is an id that is not "sbx:<ULID>".
var ErrSandboxID = errors.New("bundle: sandbox id must be sbx:<ULID>")

// ContainerID maps a sandbox id ("sbx:<ULID>") to the runsc container id and
// directory name ("sbx-<ULID>"). Anything else is refused, so no id can
// name "images", "runsc", ".", ".." or contain a slash.
func ContainerID(id string) (string, error) {
	if !sandboxIDRe.MatchString(id) {
		return "", fmt.Errorf("%w: %q", ErrSandboxID, id)
	}
	return "sbx-" + strings.TrimPrefix(id, "sbx:"), nil
}

var containerIDRe = regexp.MustCompile(`^sbx-[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)

// validContainerID reports whether a runsc id or directory name is one of
// ours.
func validContainerID(cid string) bool { return containerIDRe.MatchString(cid) }

func (b *Backend) runsc(network, overlay string) Runsc {
	r := b.cfg.Runsc
	r.Network = network
	r.Overlay = overlay
	return r
}

// proxyAddr is where sandboxes reach the egress proxy.
func (b *Backend) proxyAddr(ctx context.Context, n Net) (netip.Addr, error) {
	if b.cfg.Mode != ModeWSL {
		return n.HostAddr, nil
	}
	h, _ := b.cfg.Host.(WSLHost)
	out, err := output(ctx, b.cfg.Runner, h.DefaultRouteArgv())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("bundle: WSL default route: %w", err)
	}
	return ParseDefaultGateway(out)
}

// ensureHostRules installs the host-wide rules (proxy port guard on Linux,
// no host-initiated connections into sandboxes) once per port.
func (b *Backend) ensureHostRules(ctx context.Context, fw Firewall, proxyPort uint16) error {
	guard := proxyPort
	if b.cfg.Mode == ModeWSL {
		guard = 0
	}
	key := fmt.Sprintf("%s/%d", fw, guard)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hostRules == key {
		return nil
	}
	if fw == FirewallNFT {
		if err := b.run(ctx, NFTScriptArgv, strings.NewReader(HostRulesNFT(guard)), nil); err != nil {
			return fmt.Errorf("bundle: host rules: %w", err)
		}
	} else {
		for _, l := range []struct {
			argv       []string
			tool, tabl string
		}{{IPTablesArgv, "iptables", ""}, {IP6TablesArgv, "ip6tables", ""}} {
			out, err := b.out(ctx, l.argv)
			if err != nil {
				return fmt.Errorf("bundle: host rules: %w", err)
			}
			for _, argv := range RulesToDelete(l.tool, l.tabl, out, IsHostRule) {
				_ = b.run(ctx, argv, nil, nil)
			}
		}
		for _, argv := range HostRulesIPTables(guard) {
			if err := b.run(ctx, argv, nil, nil); err != nil {
				return fmt.Errorf("bundle: host rules: %w", err)
			}
		}
	}
	b.hostRules = key
	return nil
}

func (b *Backend) setLive(cid string, live bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if live {
		b.live[cid] = true
	} else {
		delete(b.live, cid)
	}
}

func (b *Backend) liveSets() (cids, shorts map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cids, shorts = map[string]bool{}, map[string]bool{}
	for cid := range b.live {
		cids[cid] = true
		shorts[ShortID("sbx:"+strings.TrimPrefix(cid, "sbx-"))] = true
	}
	return cids, shorts
}

// GC removes what earlier daemon runs left behind and no live sandbox owns:
// runsc containers named sbx-* (and their directories), mycel-* network
// namespaces, myh* links, inet mycel_* nft tables and MYCEL-<id> iptables
// chains with the rules that jump to them. It runs once before the first
// Create; every step is best effort and the joined errors are returned.
func (b *Backend) GC(ctx context.Context) error {
	cids, shorts := b.liveSets()
	var errs []error
	try := func(argv []string) {
		if err := b.run(ctx, argv, nil, nil); err != nil && !notExist(err) {
			errs = append(errs, err)
		}
	}
	rs := b.runsc("none", "")
	if out, err := b.out(ctx, rs.List()); err == nil {
		for _, cid := range strings.Fields(string(out)) {
			if !validContainerID(cid) || cids[cid] {
				continue
			}
			try(rs.KillAll(cid, "KILL"))
			try(rs.Delete(cid))
			try(RemoveAllArgv(b.cfg.StateDir + "/" + cid))
		}
	}
	dead := func(s string) bool { return s != "" && !shorts[s] }
	if out, err := b.out(ctx, NFTTablesArgv); err == nil {
		for _, s := range ParseNFTTables(out) {
			if dead(s) {
				try([]string{"nft", "delete", "table", "inet", "mycel_" + s})
			}
		}
	}
	match := func(l string) bool { return dead(sandboxRuleShort(l)) }
	for _, l := range []struct {
		argv        []string
		tool, table string
	}{{IPTablesNatArgv, "iptables", "nat"}, {IPTablesArgv, "iptables", ""}, {IP6TablesArgv, "ip6tables", ""}} {
		if out, err := b.out(ctx, l.argv); err == nil {
			for _, argv := range RulesToDelete(l.tool, l.table, out, match) {
				try(argv)
			}
		}
	}
	if out, err := b.out(ctx, LinkListArgv); err == nil {
		for _, s := range ParseLinks(out) {
			if dead(s) {
				try([]string{"ip", "link", "del", "myh" + s})
			}
		}
	}
	if out, err := b.out(ctx, NetnsListArgv); err == nil {
		for _, s := range ParseNetns(out) {
			if dead(s) {
				try([]string{"ip", "netns", "del", "mycel-" + s})
			}
		}
	}
	return errors.Join(errs...)
}

// notExist reports whether a failed cleanup command failed only because the
// object is already gone (judged by its stderr).
func notExist(err error) bool {
	var ee *ExitError
	if !errors.As(err, &ee) {
		return false
	}
	s := strings.ToLower(ee.Stderr)
	for _, m := range []string{
		"does not exist", "no such file", "no such container", "cannot find device",
		"not found", "no chain/target/match", "does a matching rule exist", "no such process",
		"not running", "bad rule",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
