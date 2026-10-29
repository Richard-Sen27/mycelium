package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brutella/dnssd"
)

// Advertising never claims the machine's own host name. A userland mDNS
// responder that announces A/AAAA records for the host name collides with
// the system responder (macOS mDNSResponder then renames the computer), so:
//
//   - macOS: the system responder registers the service (`dns-sd -R`, a
//     child process kept alive while advertising); there is no fallback;
//   - Linux: avahi registers it when `avahi-publish-service` exists (the
//     userland responder takes over when avahi exits within a second, e.g.
//     without an avahi daemon); otherwise the userland responder;
//   - Windows (and Linux without avahi): the userland responder
//     (github.com/brutella/dnssd), whose SRV target is a dedicated host
//     name `mycel-hub-<first 8 hex of the CA fingerprint>.local` and which
//     answers only for that name, the service type and the instance.
//
// The child runs under a small shell supervisor that kills it when its
// standard input, a pipe from the hub, closes, so a hub that dies never
// leaves a registration behind. The child's pid is recorded in the hub's
// mdns.pid, and a stale one is stopped when the hub starts again. A child
// that exits is logged with its stderr and restarted with backoff.

// Seams for tests.
var (
	goos     = runtime.GOOS
	lookPath = exec.LookPath
)

// txtKeys is the order of the TXT strings on a command line.
var txtKeys = []string{txtTrustDomain, txtURL, txtBus, txtFingerprint, txtVersion}

func txtStrings(txt map[string]string) []string {
	out := make([]string, 0, len(txtKeys))
	for _, k := range txtKeys {
		out = append(out, k+"="+txt[k])
	}
	return out
}

// advertiseCommand returns the system-responder command for this
// platform, or "" when the userland responder is to be used.
func advertiseCommand(name string, port int, txt map[string]string) (string, []string, error) {
	switch goos {
	case "darwin":
		p, err := lookPath("dns-sd")
		if err != nil {
			// Never fall back to a userland responder on macOS.
			return "", nil, errors.New("discovery: dns-sd not found; mDNS advertising needs the system responder on macOS")
		}
		args := append([]string{"-R", name, ServiceType, domain, strconv.Itoa(port)}, txtStrings(txt)...)
		return p, args, nil
	case "linux":
		p, err := lookPath("avahi-publish-service")
		if err != nil {
			return "", nil, nil
		}
		args := append([]string{"--", name, ServiceType, strconv.Itoa(port)}, txtStrings(txt)...)
		return p, args, nil
	}
	return "", nil, nil
}

// dedicatedHost is the SRV target of the userland responder.
func dedicatedHost(fp string) string { return "mycel-hub-" + fp[:8] }

func advertise(ctx context.Context, h Hub, opts AdvertiseOptions) (Advertiser, error) {
	txt, err := txtFor(h)
	if err != nil {
		return nil, err
	}
	port := h.Port
	if port == 0 {
		u, _ := url.Parse(h.URL)
		if port, err = strconv.Atoi(u.Port()); err != nil || port == 0 {
			port = 443
		}
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("discovery: invalid port %d", port)
	}
	var ips []net.IP
	for _, a := range h.Addresses {
		ip := net.ParseIP(a)
		if ip == nil {
			return nil, fmt.Errorf("discovery: invalid address %q", a)
		}
		ips = append(ips, ip)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	userland := func(actx context.Context) (Advertiser, error) {
		srv, err := userlandService(h.TrustDomain, port, txt, ips)
		if err != nil {
			return nil, err
		}
		return startUserland(actx, srv)
	}
	path, args, err := advertiseCommand(h.TrustDomain, port, txt)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return userland(ctx)
	}
	if opts.PIDFile != "" {
		killStale(opts.PIDFile, log)
	}
	var fallback func(context.Context) (Advertiser, error)
	if goos == "linux" {
		fallback = userland
	}
	return startSupervised(ctx, path, args, opts.PIDFile, log, fallback)
}

// startUserland starts the userland responder; tests replace it so they
// never announce on the network.
var startUserland = startResponder

type advertiser struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (a *advertiser) Close() error {
	a.once.Do(func() {
		a.cancel()
		t := time.NewTimer(2 * closeTimeout)
		defer t.Stop()
		select {
		case <-a.done:
		case <-t.C:
		}
	})
	return nil
}

// ---- system responder (supervised child process) ----

// supervisorScript runs the responder command ("$@", never interpolated
// into the script) in the background with its input and output detached,
// prints its pid, and kills it as soon as the supervisor's standard input
// (a pipe from the hub, kept on fd 3 because background jobs of a
// non-interactive shell read /dev/null) reaches end of file: when the hub
// stops advertising, and also when the hub dies without cleaning up, since
// the kernel then closes the pipe. The responder's stderr stays the
// supervisor's.
const supervisorScript = `exec 3<&0
"$@" </dev/null >/dev/null 3<&- &
c=$!
echo "$c"
(cat <&3 >/dev/null; kill "$c" 2>/dev/null) >/dev/null 2>&1 &
exec 3<&-
wait "$c"`

// Restart policy of the responder child (tests shorten it).
var (
	restartMin   = time.Second
	restartMax   = time.Minute
	stableAfter  = time.Minute     // a run this long resets the backoff
	quickFailure = 5 * time.Second // avahi exiting this fast (a missing daemon fails in milliseconds): use the userland responder
	pidWait      = 5 * time.Second // for the supervisor to report the child's pid
)

const stderrTail = 2048 // bytes of the child's stderr kept for the log

// child is one run of the supervisor.
type child struct {
	cmd    *exec.Cmd
	stdin  *os.File // write end of the supervisor's stdin; closing it ends the child
	pid    int      // the responder's pid (0 if unknown)
	stderr *tailBuffer
	exited chan struct{}
	err    error // set before exited closes
}

func startChildOnce(path string, args []string) (*child, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	sargs := append([]string{"-c", supervisorScript, "--", path}, args...)
	cmd := exec.Command("/bin/sh", sargs...) //nolint:gosec // G204: fixed script; the responder and its arguments are positional parameters, never parsed by the shell
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	tail := &tailBuffer{max: stderrTail}
	cmd.Stdin, cmd.Stderr = r, tail
	cmd.SysProcAttr = childAttr()
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, fmt.Errorf("discovery: start %s: %w", filepath.Base(path), err)
	}
	_ = r.Close()
	c := &child{cmd: cmd, stdin: w, stderr: tail, exited: make(chan struct{})}
	pidCh := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		pid := 0
		if sc.Scan() {
			pid, _ = strconv.Atoi(strings.TrimSpace(sc.Text()))
		}
		pidCh <- pid
		_, _ = io.Copy(io.Discard, out)
	}()
	go func() {
		c.err = cmd.Wait()
		_ = w.Close()
		close(c.exited)
	}()
	select {
	case c.pid = <-pidCh:
	case <-time.After(pidWait):
	}
	return c, nil
}

// stop closes the supervisor's stdin (it then kills the responder and
// exits) and waits; a supervisor that does not exit is killed.
func (c *child) stop() {
	_ = c.stdin.Close()
	select {
	case <-c.exited:
		return
	case <-time.After(closeTimeout):
	}
	_ = c.cmd.Process.Kill()
	<-c.exited
}

// startSupervised runs path args under the supervisor and keeps it
// running: when the responder exits it logs the exit with the tail of its
// stderr and starts it again with backoff while ctx lives. With fallback
// (Linux), a responder that exits within quickFailure is replaced by the
// fallback advertiser for the rest of ctx.
func startSupervised(ctx context.Context, path string, args []string, pidFile string, log *slog.Logger, fallback func(context.Context) (Advertiser, error)) (Advertiser, error) {
	c, err := startChildOnce(path, args)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithCancel(ctx)
	a := &advertiser{cancel: cancel, done: make(chan struct{})}
	name := filepath.Base(path)
	go func() {
		defer close(a.done)
		backoff := restartMin
		for {
			started := time.Now()
			writePID(pidFile, c.pid, log)
			select {
			case <-actx.Done():
				c.stop()
				removePID(pidFile)
				return
			case <-c.exited:
			}
			ran := time.Since(started)
			removePID(pidFile)
			log.Warn("mDNS responder exited", "cmd", name, "after", ran.Round(time.Millisecond), "err", c.err, "stderr", c.stderr.String())
			if fallback != nil && ran < quickFailure {
				log.Warn("mDNS: " + name + " exits at once; advertising with the built-in responder instead")
				fa, err := fallback(actx)
				if err != nil {
					log.Warn("mDNS advertising unavailable", "err", err)
					<-actx.Done()
					return
				}
				<-actx.Done()
				_ = fa.Close()
				return
			}
			if ran >= stableAfter {
				backoff = restartMin
			}
			for {
				t := time.NewTimer(backoff)
				select {
				case <-actx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				backoff = min(backoff*2, restartMax)
				nc, err := startChildOnce(path, args)
				if err == nil {
					c = nc
					break
				}
				log.Warn("mDNS responder restart failed", "cmd", name, "err", err, "retry_in", backoff)
			}
		}
	}()
	return a, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// ---- pid file ----

// responderNames are the commands a pid file may name; anything else at
// that pid is left alone.
var responderNames = []string{"dns-sd", "avahi-publish-service"}

func writePID(file string, pid int, log *slog.Logger) {
	if file == "" || pid <= 0 {
		return
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		log.Warn("mDNS: write pid file", "err", err)
	}
}

func removePID(file string) {
	if file != "" {
		_ = os.Remove(file)
	}
}

// killStale ends a responder left behind by a hub that died (its pid is in
// file) when that process still runs our registration command.
func killStale(file string, log *slog.Logger) {
	b, err := os.ReadFile(file) //nolint:gosec // the hub's own pid file
	if err != nil {
		return
	}
	defer removePID(file)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	cmdline := processCommand(pid)
	if !strings.Contains(cmdline, ServiceType) || !slices.ContainsFunc(responderNames, func(n string) bool { return strings.Contains(cmdline, n) }) {
		return
	}
	if err := terminate(pid); err == nil {
		log.Warn("mDNS: stopped a responder left behind by an earlier hub", "pid", pid)
	}
}

// processCommand returns the command line of pid, or "".
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output() //nolint:gosec // G204: numeric pid
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---- userland responder ----

// userlandService builds the service for the userland responder: its
// host name is the dedicated name, never the machine's.
func userlandService(name string, port int, txt map[string]string, ips []net.IP) (dnssd.Service, error) {
	srv, err := dnssd.NewService(dnssd.Config{
		Name: name, Type: ServiceType, Domain: domain, Host: dedicatedHost(txt[txtFingerprint]),
		Text: txt, Port: port, IPs: ips, Ifaces: lanInterfaces(),
	})
	if err != nil {
		return dnssd.Service{}, fmt.Errorf("discovery: %w", err)
	}
	return srv, nil
}

// answeredNames are the query names the userland responder answers for a
// service (github.com/brutella/dnssd v1.2.14, responder.handleQuestion):
// the service type, the instance, the service's host name and the DNS-SD
// meta query. Probing and announcing use the same names.
func answeredNames(srv dnssd.Service) []string {
	return []string{srv.ServiceName(), srv.EscapedServiceInstanceName(), srv.Hostname(), srv.ServicesMetaQueryName()}
}

func startResponder(ctx context.Context, srv dnssd.Service) (Advertiser, error) {
	quiet()
	rp, err := dnssd.NewResponder()
	if err != nil {
		return nil, fmt.Errorf("discovery: mDNS responder: %w", err)
	}
	if _, err := rp.Add(srv); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	actx, cancel := context.WithCancel(ctx)
	a := &advertiser{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		_ = rp.Respond(actx) // returns the context error when stopped
	}()
	return a, nil
}

// lanInterfaces lists the interfaces worth announcing on: up, multicast,
// not a point-to-point tunnel (VPNs), and holding an IPv4 address (this
// also skips interfaces without addresses, where multicast writes stall
// for seconds and delay the announcement). Loopback stays, so a node on
// the hub machine finds it too. nil (every multicast interface) when none
// qualifies.
func lanInterfaces() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, i.Name)
				break
			}
		}
	}
	return out
}
