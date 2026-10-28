package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Runner defaults.
const (
	DefaultStatsInterval  = 5 * time.Second
	DefaultKillGrace      = 5 * time.Second
	DefaultMaxLogBytes    = 64 << 20
	DefaultMaxOutputBytes = 1 << 30
)

// Internal bounds; variables so tests can shorten them.
var (
	// drainTimeout bounds each wait for stdout/stderr to reach EOF after the
	// command's process group is gone: once before and once after Destroy;
	// then the copiers are abandoned.
	drainTimeout = 2 * time.Second
	// collectTimeout bounds output collection.
	collectTimeout = 2 * time.Minute
	// statsCallTimeout bounds one Stats call.
	statsCallTimeout = 10 * time.Second
)

// Runner executes one job against a backend: prepare, create, exec, stream
// logs, enforce the wall clock, collect outputs, destroy.
type Runner struct {
	Backend Backend
	// Logs receives stdout and stderr interleaved as raw bytes, as they
	// arrive; nil discards. Writes are serialised and stop when Run returns.
	Logs io.Writer
	// OnStats is called with every resource sample (optional).
	OnStats func(Stats)
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// StatsInterval is the sampling period; zero means DefaultStatsInterval.
	StatsInterval time.Duration
	// KillGrace is the time between TERM and KILL, and between KILL and
	// giving up; zero means DefaultKillGrace.
	KillGrace time.Duration
	// MaxLogBytes caps what reaches Logs; zero means DefaultMaxLogBytes.
	MaxLogBytes int64
	// MaxOutputBytes caps the total size of collected outputs; zero means
	// DefaultMaxOutputBytes.
	MaxOutputBytes int64
}

// Result is the outcome of one Run.
type Result struct {
	ExitCode int
	State    mycelv1.JobState
	Error    string
	Outputs  []OutputFile
	Stats    Stats
}

// OutputFile is one collected output: Path relative to /workspace (slash
// separated), LocalPath under <spec.Dir>/outputs.
type OutputFile struct {
	Path      string
	LocalPath string
	Size      int64
}

// LogTruncatedMarker is the last line written to Logs when the cap is hit.
const LogTruncatedMarker = "\n[mycel] log truncated: size limit reached\n"

// ErrKillFailed is returned by Run when the command survived KILL.
var ErrKillFailed = errors.New("sandbox: kill failed")

// Run executes argv in a new sandbox described by spec and returns the
// terminal state. The sandbox's own scratch directory is <spec.Dir>/sandbox
// (removed by Destroy); collected outputs land in <spec.Dir>/outputs, which
// is cleared first.
//
// timeout bounds the command; zero means spec.Limits.WallClock, a non-zero
// spec.Limits.WallClock also caps a larger timeout, and when both are zero
// DefaultWallClock applies. When it fires the process group gets TERM, then
// KILL after the grace period, and the state is TIMED_OUT. Cancelling ctx
// does the same and yields CANCELLED. A backend reporting ErrTimeout also
// yields TIMED_OUT.
//
// After the command exits its whole process group is killed, then outputs
// matching the globs are collected (up to MaxOutputBytes in total). A
// CANCELLED job yields no outputs.
//
// The returned error is non-nil only when the sandbox could not be set up
// (prepare, create or exec failed) or the command survived KILL
// (ErrKillFailed); Result is filled in either way. A non-zero exit is a
// FAILED result with a nil error.
func (r *Runner) Run(ctx context.Context, spec Spec, argv []string, outputs []string, timeout time.Duration) (Result, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	res := Result{ExitCode: -1, State: mycelv1.JobState_JOB_STATE_FAILED}
	fail := func(err error) (Result, error) {
		if ctx.Err() != nil {
			res.State = mycelv1.JobState_JOB_STATE_CANCELLED
		}
		res.Error = err.Error()
		return res, err
	}
	if r.Backend == nil {
		return fail(errors.New("sandbox: runner has no backend"))
	}
	if len(argv) == 0 {
		return fail(errors.New("sandbox: empty command"))
	}
	if spec.Dir == "" || !filepath.IsAbs(spec.Dir) {
		return fail(fmt.Errorf("sandbox: spec.Dir %q must be an absolute path", spec.Dir))
	}
	for _, o := range outputs {
		if err := checkRelPattern(o); err != nil {
			return fail(err)
		}
	}
	if w := spec.Limits.WallClock; w > 0 && (timeout <= 0 || timeout > w) {
		timeout = w
	}
	if timeout <= 0 {
		timeout = DefaultWallClock
	}
	grace := r.KillGrace
	if grace <= 0 {
		grace = DefaultKillGrace
	}
	outDir := filepath.Join(spec.Dir, "outputs")
	bspec := spec
	bspec.Dir = filepath.Join(spec.Dir, "sandbox")
	if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
		return fail(err)
	}

	if err := r.Backend.Prepare(ctx, spec.Image, func(string) {}); err != nil {
		return fail(fmt.Errorf("sandbox: prepare %s: %w", spec.Image, err))
	}
	sb, err := r.Backend.Create(ctx, bspec)
	if err != nil {
		return fail(fmt.Errorf("sandbox: create: %w", err))
	}
	// Cleanup must run even when ctx is cancelled.
	bg := context.WithoutCancel(ctx)
	defer func() { _ = sb.Destroy(bg) }()

	started := now()
	proc, err := sb.Exec(bg, argv, ExecOpts{Dir: WorkspaceGuestPath})
	if err != nil {
		return fail(fmt.Errorf("sandbox: exec: %w", err))
	}

	logs := &capWriter{w: r.Logs, max: r.MaxLogBytes}
	if logs.w == nil {
		logs.w = io.Discard
	}
	if logs.max <= 0 {
		logs.max = DefaultMaxLogBytes
	}
	defer logs.abandon()
	var copiers sync.WaitGroup
	for _, rd := range []io.Reader{proc.Stdout(), proc.Stderr()} {
		if rd == nil {
			continue
		}
		copiers.Go(func() { _, _ = io.Copy(logs, rd) })
	}
	drained := make(chan struct{})
	go func() { copiers.Wait(); close(drained) }()
	drain := func() {
		select {
		case <-drained:
			return
		case <-time.After(drainTimeout):
		}
		_ = sb.Destroy(bg)
		select {
		case <-drained:
		case <-time.After(drainTimeout):
			logs.abandon() // a reader that never ends: give up on it
		}
	}

	type waitResult struct {
		code int
		err  error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		code, err := proc.Wait()
		waitCh <- waitResult{code, err}
	}()

	// Stats sampling.
	var statsMu sync.Mutex
	var last Stats
	sample := func() {
		c, cancel := context.WithTimeout(bg, statsCallTimeout)
		s, err := sb.Stats(c)
		cancel()
		if err != nil {
			return
		}
		statsMu.Lock()
		if s.MemoryBytes < last.MemoryBytes {
			s.MemoryBytes = last.MemoryBytes
		}
		if s.RunningSince.IsZero() {
			s.RunningSince = started
		}
		last = s
		statsMu.Unlock()
		if r.OnStats != nil {
			r.OnStats(s)
		}
	}
	interval := r.StatsInterval
	if interval <= 0 {
		interval = DefaultStatsInterval
	}
	statsDone := make(chan struct{})
	var statsWG sync.WaitGroup
	statsWG.Go(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-statsDone:
				return
			case <-t.C:
				sample()
			}
		}
	})
	stopStats := sync.OnceFunc(func() { close(statsDone); statsWG.Wait() })
	defer stopStats()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var killTimer, giveUpTimer *time.Timer
	defer func() {
		if killTimer != nil {
			killTimer.Stop()
		}
		if giveUpTimer != nil {
			giveUpTimer.Stop()
		}
	}()
	var (
		wr                         waitResult
		exited, stopping, killSent bool
		timedOut, cancelled        bool
		deadlineC                  = deadline.C
		ctxDone                    = ctx.Done()
		killC, giveUpC             <-chan time.Time
		waitC                      = waitCh
	)
	// exitedNow reports whether the command already finished, so a timer
	// racing with a normal exit does not mislabel it.
	exitedNow := func() bool {
		select {
		case wr = <-waitCh:
			exited, waitC = true, nil
			return true
		default:
			return false
		}
	}
	beginStop := func() {
		stopping = true
		deadlineC, ctxDone = nil, nil
		_ = proc.Signal("TERM")
		killTimer = time.NewTimer(grace)
		killC = killTimer.C
	}
	sendKill := func() {
		killSent = true
		killC = nil
		_ = proc.Signal("KILL")
	}
loop:
	for {
		select {
		case wr = <-waitC:
			exited, waitC = true, nil
			// While stopping, keep the KILL timer: children of the leader
			// get the rest of the grace period, then the group is killed.
			if !stopping || killSent {
				break loop
			}
		case <-deadlineC:
			if exitedNow() {
				break loop
			}
			timedOut = true
			beginStop()
		case <-ctxDone:
			if exitedNow() {
				break loop
			}
			cancelled = true
			beginStop()
		case <-killC:
			sendKill()
			if exited {
				break loop
			}
			giveUpTimer = time.NewTimer(grace)
			giveUpC = giveUpTimer.C
		case <-giveUpC:
			// The leader survived KILL: tear the sandbox down and report.
			_ = sb.Destroy(bg)
			stopStats()
			statsMu.Lock()
			res.Stats = last
			statsMu.Unlock()
			res.State = mycelv1.JobState_JOB_STATE_FAILED
			res.Error = "kill failed: the command did not exit after KILL"
			drain()
			return res, ErrKillFailed
		}
	}
	// Kill whatever the command left behind before reading outputs.
	if !killSent {
		_ = proc.Signal("KILL")
	}

	stopStats()
	sample()
	statsMu.Lock()
	res.Stats = last
	statsMu.Unlock()

	res.ExitCode = wr.code
	switch {
	case timedOut || errors.Is(wr.err, ErrTimeout):
		res.State = mycelv1.JobState_JOB_STATE_TIMED_OUT
		res.Error = fmt.Sprintf("%v after %s", ErrTimeout, timeout)
		if !timedOut {
			res.Error = wr.err.Error()
		}
	case cancelled:
		res.State = mycelv1.JobState_JOB_STATE_CANCELLED
		res.Error = "cancelled"
	case wr.err != nil:
		res.State = mycelv1.JobState_JOB_STATE_FAILED
		res.Error = wr.err.Error()
	case wr.code == 0:
		res.State = mycelv1.JobState_JOB_STATE_SUCCEEDED
	default:
		res.State = mycelv1.JobState_JOB_STATE_FAILED
		res.Error = fmt.Sprintf("exit code %d", wr.code)
	}

	if len(outputs) > 0 && !cancelled {
		limit := r.MaxOutputBytes
		if limit <= 0 {
			limit = DefaultMaxOutputBytes
		}
		cctx, cancel := context.WithTimeout(bg, collectTimeout)
		files, err := collectOutputs(cctx, sb, outputs, outDir, limit)
		cancel()
		res.Outputs = files
		if err != nil {
			msg := "collect outputs: " + err.Error()
			if res.Error != "" {
				msg = res.Error + "; " + msg
			}
			res.Error = msg
		}
	}

	drain()
	return res, nil
}

// capWriter forwards writes until max bytes, then writes the marker once and
// discards the rest. It never returns an error to the copier, so the pipe
// keeps draining and the command does not block. After abandon it drops
// everything, so nothing reaches the caller's writer once Run returned.
type capWriter struct {
	mu        sync.Mutex
	w         io.Writer
	max       int64
	n         int64
	truncated bool
	gone      bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated || c.gone {
		return len(p), nil
	}
	if rem := c.max - c.n; int64(len(p)) > rem {
		_, _ = c.w.Write(p[:rem])
		c.n = c.max
		_, _ = io.WriteString(c.w, LogTruncatedMarker)
		c.truncated = true
		return len(p), nil
	}
	_, _ = c.w.Write(p)
	c.n += int64(len(p))
	return len(p), nil
}

func (c *capWriter) abandon() {
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
}

func checkRelPattern(p string) error {
	if p == "" {
		return errors.New("sandbox: empty output pattern")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || filepath.IsAbs(p) {
		return fmt.Errorf("sandbox: output pattern %q must be relative to /workspace", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("sandbox: output pattern %q escapes /workspace", p)
		}
	}
	return nil
}

func collectOutputs(ctx context.Context, sb Sandbox, patterns []string, outDir string, limit int64) ([]OutputFile, error) {
	if err := os.RemoveAll(outDir); err != nil {
		return nil, err
	}
	paths, err := sb.Glob(ctx, patterns)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []OutputFile
	var errs []error
	seen := map[string]bool{}
	remaining := limit
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		local := filepath.FromSlash(p)
		if !filepath.IsLocal(local) {
			errs = append(errs, fmt.Errorf("%q: not a local path", p))
			continue
		}
		dst := filepath.Join(outDir, local)
		n, err := copyOutFile(ctx, sb, p, dst, remaining)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			if errors.Is(err, ErrLimit) {
				break
			}
			continue
		}
		remaining -= n
		out = append(out, OutputFile{Path: p, LocalPath: dst, Size: n})
	}
	return out, errors.Join(errs...)
}

func copyOutFile(ctx context.Context, sb Sandbox, p, dst string, remaining int64) (int64, error) {
	rc, err := sb.CopyOut(ctx, p)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyN(f, rc, remaining+1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > remaining {
		err = fmt.Errorf("%w: outputs exceed the total size limit", ErrLimit)
	}
	if err != nil {
		_ = os.Remove(dst)
		return 0, err
	}
	return n, nil
}
