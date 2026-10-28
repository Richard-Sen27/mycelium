package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// WorkersDir holds per-worker files (mcp.json, stderr.log) under NodeDir.
const WorkersDir = "workers"

// Worker limits.
const (
	maxWorkers   = 8
	maxTaskBytes = 16 << 10 // argv limits on Windows (32 K chars for the whole command line)
	maxSpawnBody = 64 << 10
	maxStderrLog = 10 << 20
	maxUsageLine = 1 << 20
	// maxChannelLen bounds the channel name; the hub validates it fully.
	maxChannelLen     = 128
	defaultMaxTurns   = 30
	maxMaxTurns       = 1000
	usageReportFinal  = 5 * time.Second // timeout of the last usage report
	exitReportTimeout = 5 * time.Second // timeout of the agent.exited report
	workerWaitDelay   = 2 * time.Second
)

// Worker states in GET /api/local/workers.
const (
	workerStarting = "starting"
	workerRunning  = "running"
	workerPaused   = "paused"
	workerExited   = "exited"
)

// Exit reasons reported with agent.exited.
const (
	exitFinished = "finished"
	exitStopped  = "stopped" // stopped on request: DELETE or AgentCtl stop
	exitKilled   = "killed"  // ended by the daemon: shutdown, revocation, agent revoked
)

// profileTrustedHost is the only worker profile of phase 3.
const profileTrustedHost = "trusted-host"

// worker is a harness process started by this daemon for a local agent.
type worker struct {
	name     string
	agentID  string
	parent   string // agent id of the spawner; kept after its session ends
	harness  string
	channel  string
	parser   string        // usage parser
	done     chan struct{} // closed when the process has exited
	finished chan struct{} // closed after the exit was reported and the session ended

	reportMu sync.Mutex // one usage report at a time

	mu         sync.Mutex
	state      string
	sess       *agentSession
	ctl        *procCtl
	pid        int
	reaped     bool // the process has been waited for: no more signals
	startedAt  time.Time
	exitedAt   time.Time
	exitCode   int
	exitReason string
	stopReason string // set when the daemon ends the process
	stopping   bool
	// Control that arrived while the worker was starting, applied right
	// after the process starts.
	pendingStop  string
	pendingPause string
	// totals parsed from stdout, and how much of them the hub has seen
	tokens, repTokens int64
	usd, repUSD       float64
	turns, repTurns   int32
	seen              map[string]struct{} // assistant message ids
}

// signal runs fn on the process group unless the process was reaped.
func (w *worker) signal(fn func(*procCtl) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctl == nil || w.reaped {
		return nil
	}
	return fn(w.ctl)
}

func (w *worker) stateNow() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// ---- spawn ----

type spawnBudget struct {
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
}

type spawnBody struct {
	Harness  string       `json:"harness"`
	Name     string       `json:"name"`
	Task     string       `json:"task"`
	Channel  string       `json:"channel"`
	Profile  string       `json:"profile"`
	Workdir  string       `json:"workdir"`
	MaxTurns int          `json:"max_turns"`
	Budget   *spawnBudget `json:"budget"`
	// Env is refused: workers get only the daemon's environment.
	Env json.RawMessage `json:"env"`
}

type spawnReply struct {
	AgentID string `json:"agent_id"`
	Worker  string `json:"worker"`
}

// validEnvName: a portable environment variable name.
func validEnvName(k string) bool {
	if k == "" || len(k) > 128 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// normaliseChannel checks a channel name and adds the '#' prefix when it is
// missing (as the MCP adapter does). The hub validates it fully.
func normaliseChannel(ch string) (string, bool) {
	ch = strings.TrimSpace(ch)
	if ch == "" || len(ch) > maxChannelLen {
		return "", false
	}
	for _, r := range ch {
		if r <= ' ' || r == 0x7f {
			return "", false
		}
	}
	if !strings.HasPrefix(ch, "#") && !strings.HasPrefix(ch, "dm:") {
		ch = "#" + ch
	}
	return ch, true
}

// validateSpawn checks the request body. It returns the HTTP status and
// message of the first problem.
func validateSpawn(b *spawnBody) (int, string) {
	if len(b.Env) > 0 {
		return http.StatusBadRequest, "env is not accepted: workers get the daemon's environment only"
	}
	if b.Profile != "" && b.Profile != profileTrustedHost {
		return http.StatusBadRequest, "profile " + strconv.Quote(b.Profile) + ": sandboxed workers arrive with phase 4; use \"trusted-host\""
	}
	if b.Name != "" && !identity.ValidNodeName(b.Name) {
		return http.StatusBadRequest, "invalid worker name: 1-63 chars of a-z, 0-9 and '-', not starting or ending with '-'"
	}
	if strings.TrimSpace(b.Task) == "" {
		return http.StatusBadRequest, "task is required"
	}
	if len(b.Task) > maxTaskBytes {
		return http.StatusBadRequest, "task is longer than 16 KiB"
	}
	if strings.ContainsRune(b.Task, 0) {
		return http.StatusBadRequest, "task contains a NUL byte"
	}
	ch, ok := normaliseChannel(b.Channel)
	if !ok {
		return http.StatusBadRequest, "channel is required (1-128 chars, no spaces)"
	}
	b.Channel = ch
	if b.MaxTurns < 0 || b.MaxTurns > maxMaxTurns {
		return http.StatusBadRequest, "max_turns must be between 1 and 1000"
	}
	if bg := b.Budget; bg != nil && (bg.Tokens < 0 || bg.USD < 0 || math.IsNaN(bg.USD) || math.IsInf(bg.USD, 0)) {
		return http.StatusBadRequest, "budget must not be negative"
	}
	if b.Workdir == "" {
		return http.StatusBadRequest, "workdir is required"
	}
	if !filepath.IsAbs(b.Workdir) || strings.ContainsRune(b.Workdir, 0) {
		return http.StatusBadRequest, "invalid workdir: must be an absolute path"
	}
	return 0, ""
}

func (l *localAPI) handleSpawn(w http.ResponseWriter, r *http.Request) {
	parent := l.session(r.PathValue("session"))
	if parent == nil {
		writeJSONError(w, http.StatusNotFound, "unknown session")
		return
	}
	if parent.worker {
		writeJSONError(w, http.StatusForbidden, "workers cannot spawn workers")
		return
	}
	if paused, reason := parent.pauseState(); paused {
		writeJSONError(w, http.StatusLocked, "agent paused: "+reason)
		return
	}
	if l.n.isFrozen() {
		writeJSONError(w, http.StatusLocked, frozenMarker)
		return
	}
	var body spawnBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpawnBody)).Decode(&body); err != nil {
		writeBodyError(w, err, "invalid JSON body")
		return
	}
	if !validHarness(body.Harness) {
		writeJSONError(w, http.StatusBadRequest, "invalid harness: 1-64 chars of a-z, 0-9 and '-'")
		return
	}
	spec, err := loadSpawnSpec(body.Harness)
	if errors.Is(err, errNoSpawn) {
		writeJSONError(w, http.StatusBadRequest, "harness "+strconv.Quote(body.Harness)+" cannot spawn workers; harnesses that can: "+strings.Join(spawnableHarnesses(), ", "))
		return
	}
	if err != nil {
		l.log.Error("harness manifest", "harness", body.Harness, "err", err)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if code, msg := validateSpawn(&body); code != 0 {
		writeJSONError(w, code, msg)
		return
	}
	if body.MaxTurns == 0 {
		body.MaxTurns = spec.MaxTurnsDefault
		if body.MaxTurns <= 0 {
			body.MaxTurns = defaultMaxTurns
		}
	}
	workdir, err := filepath.EvalSymlinks(body.Workdir)
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(workdir); err == nil && !fi.IsDir() {
			err = errors.New("not a directory")
		}
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "workdir "+strconv.Quote(body.Workdir)+" is not an existing directory")
		return
	}

	wk, code, msg := l.reserveWorker(parent, &body, spec)
	if wk == nil {
		writeJSONError(w, code, msg)
		return
	}
	ok := false
	defer func() {
		if !ok {
			l.dropWorker(wk)
		}
	}()

	ctx, cancel := l.requestContext(r.Context(), localRPCTimeout)
	defer cancel()
	unlock, err := l.regLocks.lock(ctx, wk.name)
	if err != nil {
		writeRPCError(w, err)
		return
	}
	defer unlock()
	l.mu.Lock()
	taken := l.byName[wk.name] != nil
	l.mu.Unlock()
	if taken {
		writeJSONError(w, http.StatusConflict, "agent "+wk.name+" already has a local session")
		return
	}

	launch, err := l.prepareLaunch(wk, &body, spec, workdir)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	reg := &mycelv1.RegisterAgentRequest{
		AgentName: wk.name, SandboxId: "", Harness: body.Harness,
		Parent: parent.agentID, HomeChannel: body.Channel, Workdir: workdir,
	}
	if body.Budget != nil {
		reg.Budget = &mycelv1.Budget{Tokens: body.Budget.Tokens, Usd: body.Budget.USD}
	}
	sess, err := l.registerSession(ctx, reg, true)
	if err != nil {
		writeRegisterError(w, err)
		return
	}
	// registerSession checked that the hub's agent id is wk.agentID and
	// flagged the session if the hub reports the agent paused.
	if paused, reason := sess.pauseState(); paused {
		l.pauseWorker(wk, reason)
	}

	if err := l.startWorker(wk, sess, launch); err != nil {
		l.log.Warn("worker did not start", "agent", wk.agentID, "err", err)
		rctx, rcancel := context.WithTimeout(l.ctx, exitReportTimeout)
		_ = l.n.hubRPC(rctx, bus.RPCAgentExited, &mycelv1.AgentExitedRequest{AgentId: wk.agentID, ExitCode: -1, Reason: "error: start failed"}, &mycelv1.AgentExitedResponse{})
		rcancel()
		l.endSession(sess)
		if errors.Is(err, errShuttingDown) {
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeJSONError(w, http.StatusInternalServerError, "could not start the harness: "+err.Error())
		}
		return
	}
	ok = true
	writeJSON(w, http.StatusCreated, spawnReply{AgentID: wk.agentID, Worker: wk.agentID})
}

// reserveWorker picks the worker's name and claims a worker slot.
func (l *localAPI) reserveWorker(parent *agentSession, b *spawnBody, spec *spawnSpec) (*worker, int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, http.StatusServiceUnavailable, errShuttingDown.Error()
	}
	l.pruneWorkersLocked()
	active := 0
	for _, w := range l.workers {
		if w.stateNow() != workerExited {
			active++
		}
	}
	if active >= maxWorkers {
		return nil, http.StatusTooManyRequests, fmt.Sprintf("at most %d workers run on a node at a time", maxWorkers)
	}
	name := b.Name
	if name == "" {
		name = l.nextWorkerNameLocked(parent.name)
		if name == "" {
			return nil, http.StatusConflict, "no free worker name"
		}
	} else {
		if old := l.workers[name]; old != nil && old.stateNow() != workerExited {
			return nil, http.StatusConflict, "worker " + name + " is already running"
		}
		if l.byName[name] != nil {
			return nil, http.StatusConflict, "agent " + name + " already has a local session"
		}
	}
	w := &worker{
		name: name, agentID: "agent:" + name + "@" + l.n.name(), parent: parent.agentID,
		harness: b.Harness, channel: b.Channel, parser: spec.Usage,
		done: make(chan struct{}), finished: make(chan struct{}), state: workerStarting,
	}
	l.workers[name] = w
	return w, 0, ""
}

// nextWorkerNameLocked returns <parent>-w<N> with the smallest free N.
func (l *localAPI) nextWorkerNameLocked(parent string) string {
	for n := 1; n <= 10000; n++ {
		suffix := "-w" + strconv.Itoa(n)
		base := parent
		if len(base)+len(suffix) > 63 {
			base = strings.TrimRight(base[:63-len(suffix)], "-")
		}
		name := base + suffix
		if l.workers[name] == nil && l.byName[name] == nil && identity.ValidNodeName(name) {
			return name
		}
	}
	return ""
}

// dropWorker removes a worker that never started.
func (l *localAPI) dropWorker(w *worker) {
	l.mu.Lock()
	if l.workers[w.name] == w {
		delete(l.workers, w.name)
	}
	l.mu.Unlock()
}

// pruneWorkersLocked forgets workers that exited more than workerKeep ago.
func (l *localAPI) pruneWorkersLocked() {
	keep := l.n.cfg.workerKeep
	for name, w := range l.workers {
		w.mu.Lock()
		old := w.state == workerExited && time.Since(w.exitedAt) > keep
		w.mu.Unlock()
		if old {
			delete(l.workers, name)
		}
	}
}

// launch is a rendered harness command line.
type launch struct {
	command string
	args    []string
	env     []string
	workdir string
	dir     string // <NodeDir>/workers/<name>
}

// mycelHome is the MYCEL_HOME a worker's `mycel mcp` needs to find this
// daemon's NodeDir: the daemon's own, or derived from a <home>/data/node
// layout. Empty when the default directories apply (the worker then
// resolves the same defaults as the daemon).
func mycelHome(nodeDir string) string {
	if h := os.Getenv(config.EnvHome); h != "" {
		if abs, err := filepath.Abs(h); err == nil {
			return abs
		}
		return h
	}
	abs, err := filepath.Abs(nodeDir)
	if err != nil {
		return ""
	}
	data := filepath.Dir(abs)
	if filepath.Base(abs) == "node" && filepath.Base(data) == "data" {
		return filepath.Dir(data)
	}
	return ""
}

// prepareLaunch renders the manifest for w and writes its MCP config.
func (l *localAPI) prepareLaunch(w *worker, b *spawnBody, spec *spawnSpec, workdir string) (*launch, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the mycel binary: %w", err)
	}
	dir := filepath.Join(l.n.dir, WorkersDir, w.name)
	mcpFile := filepath.Join(dir, "mcp.json")
	v := renderVars{vals: map[string]string{
		"agent":           w.name,
		"parent":          w.parent,
		"channel":         b.Channel,
		"task":            b.Task,
		"harness":         b.Harness,
		"max_turns":       strconv.Itoa(b.MaxTurns),
		"mycel":           exe,
		"mcp_config_file": mcpFile,
	}}
	prompt, err := v.render(spec.Prompt)
	if err != nil {
		return nil, fmt.Errorf("harness manifest prompt: %w", err)
	}
	v.vals["prompt"] = prompt
	command, err := v.render(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("harness manifest command: %w", err)
	}
	if _, err := exec.LookPath(command); err != nil {
		return nil, fmt.Errorf("harness command %q not found on this machine", command)
	}
	args := make([]string, 0, len(spec.Args))
	for _, a := range spec.Args {
		ra, err := v.render(a)
		if err != nil {
			return nil, fmt.Errorf("harness manifest args: %w", err)
		}
		args = append(args, ra)
	}
	tree, err := v.renderTree(spec.MCPConfig)
	if err != nil {
		return nil, fmt.Errorf("harness manifest mcp_config: %w", err)
	}
	cfg, _ := tree.(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	addLocalAddr(cfg, exe, l.n.localAddr)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode mcp config: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create worker directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !isWindows() { // #nosec G302 -- a directory, owner only
		return nil, fmt.Errorf("worker directory: %w", err)
	}
	if err := writeFileAtomic(mcpFile, data); err != nil {
		return nil, fmt.Errorf("write mcp config: %w", err)
	}

	// The daemon's environment, then MYCEL_HOME and MYCEL_LOCAL_ADDR last
	// so that nothing can point the worker at another daemon.
	env := os.Environ()
	if h := mycelHome(l.n.dir); h != "" {
		env = append(env, config.EnvHome+"="+h)
	}
	env = append(env, "MYCEL_LOCAL_ADDR="+l.n.localAddr)
	return &launch{command: command, args: args, env: env, workdir: workdir, dir: dir}, nil
}

// startWorker starts the harness process and the goroutines that watch it.
func (l *localAPI) startWorker(w *worker, sess *agentSession, ln *launch) error {
	if hook := l.n.cfg.workerStartHook; hook != nil {
		hook(w.name)
	}
	stderrF, err := os.OpenFile(filepath.Join(ln.dir, "stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open stderr.log: %w", err)
	}
	wctx, wcancel := context.WithCancel(l.ctx)
	cmd := exec.CommandContext(wctx, ln.command, ln.args...) // #nosec G204 -- the command comes from the harness manifest
	cmd.Dir = ln.workdir
	cmd.Env = ln.env
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = &capWriter{w: stderrF, left: maxStderrLog}
	setProcAttrs(cmd)
	cmd.Cancel = func() error { return w.signal((*procCtl).kill) }
	cmd.WaitDelay = workerWaitDelay
	fail := func() {
		_ = pr.Close()
		wcancel()
		_ = stderrF.Close()
	}
	if err := cmd.Start(); err != nil {
		fail()
		return err
	}
	ctl, err := newProcCtl(cmd.Process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fail()
		return fmt.Errorf("process control: %w", err)
	}

	// The closed check and the switch to running happen together, so a
	// shutdown either sees this worker running or this start sees it.
	l.mu.Lock()
	closed := l.closed
	var pendStop, pendPause string
	if !closed {
		l.wg.Add(3)
		w.mu.Lock()
		w.sess, w.ctl, w.pid, w.startedAt, w.state = sess, ctl, cmd.Process.Pid, time.Now().UTC(), workerRunning
		pendStop, pendPause = w.pendingStop, w.pendingPause
		w.mu.Unlock()
	}
	l.mu.Unlock()
	if closed {
		_ = ctl.kill()
		_ = cmd.Wait()
		ctl.release()
		fail()
		return errShuttingDown
	}
	l.log.Info("worker started", "agent", w.agentID, "harness", w.harness, "pid", cmd.Process.Pid, "parent", w.parent)

	parsed := make(chan struct{})
	go func() {
		defer l.wg.Done()
		defer close(parsed)
		l.parseUsage(w, pr)
	}()
	go func() {
		defer l.wg.Done()
		l.usageLoop(w)
	}()
	go func() {
		defer l.wg.Done()
		defer wcancel()
		waitErr := cmd.Wait()
		// Best effort: end whatever the harness left in its group, then
		// never signal this group again (its id may be reused).
		w.mu.Lock()
		_ = w.ctl.kill()
		w.ctl.release()
		w.reaped = true
		w.mu.Unlock()
		_ = pw.Close()
		<-parsed
		_ = stderrF.Close()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		} else if waitErr != nil {
			l.log.Warn("worker wait", "agent", w.agentID, "err", waitErr)
		}
		l.workerExited(w, code)
	}()

	switch {
	case pendStop != "":
		l.stopWorker(w, pendStop)
	case pendPause != "":
		l.pauseWorker(w, pendPause)
	}
	if canPause && l.n.isFrozen() {
		// The node froze while the worker was starting.
		_ = w.signal((*procCtl).pause)
	}
	return nil
}

// freezeWorkers stops (on) or continues (off) every worker process for a
// network freeze. Workers paused by the hub stay stopped on Unfreeze. A
// freeze makes their sessions refuse write RPCs through the frozen check
// of the local API, not through the pause flag, so a hub pause and a
// freeze never undo each other.
func (l *localAPI) freezeWorkers(on bool) {
	l.mu.Lock()
	list := make([]*worker, 0, len(l.workers))
	for _, w := range l.workers {
		list = append(list, w)
	}
	l.mu.Unlock()
	for _, w := range list {
		w.mu.Lock()
		state, stopping := w.state, w.stopping
		w.mu.Unlock()
		if !canPause || stopping {
			continue
		}
		switch {
		case on && (state == workerRunning || state == workerPaused):
			if err := w.signal((*procCtl).pause); err != nil {
				l.log.Warn("freeze worker", "agent", w.agentID, "err", err)
			}
		case !on && state == workerRunning:
			if err := w.signal((*procCtl).resume); err != nil {
				l.log.Warn("unfreeze worker", "agent", w.agentID, "err", err)
			}
		}
	}
	if len(list) > 0 {
		l.log.Info("workers "+map[bool]string{true: "frozen", false: "unfrozen"}[on], "workers", len(list))
	}
}

// workerExited records the exit, reports the last usage and the exit to the
// hub (best effort) and ends the worker's session.
func (l *localAPI) workerExited(w *worker, code int) {
	w.mu.Lock()
	reason := w.stopReason
	if reason == "" {
		if code == 0 {
			reason = exitFinished
		} else {
			reason = "error: exit " + strconv.Itoa(code)
		}
	}
	w.state, w.exitedAt, w.exitCode, w.exitReason = workerExited, time.Now().UTC(), code, reason
	sess := w.sess
	w.mu.Unlock()
	close(w.done)
	l.log.Info("worker exited", "agent", w.agentID, "code", code, "reason", reason)

	uctx, ucancel := context.WithTimeout(l.ctx, usageReportFinal)
	l.reportUsage(uctx, w)
	ucancel()
	ectx, ecancel := context.WithTimeout(l.ctx, exitReportTimeout)
	err := l.n.hubRPC(ectx, bus.RPCAgentExited, &mycelv1.AgentExitedRequest{
		AgentId: w.agentID, ExitCode: int32(max(min(code, math.MaxInt32), math.MinInt32)), Reason: reason, // #nosec G115 -- clamped
	}, &mycelv1.AgentExitedResponse{})
	ecancel()
	if err != nil {
		l.log.Warn("report worker exit", "agent", w.agentID, "err", err)
	}
	if sess != nil {
		l.endSession(sess)
	}
	close(w.finished)
}

// capWriter writes up to left bytes and silently drops the rest.
type capWriter struct {
	w    io.Writer
	left int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.left > 0 {
		n := min(int64(len(p)), c.left)
		if _, err := c.w.Write(p[:n]); err != nil {
			c.left = 0
		} else {
			c.left -= n
		}
	}
	return len(p), nil
}

// ---- control ----

// pauseWorker suspends a running worker and refuses its local RPCs. On a
// starting worker the pause is applied right after the process starts.
func (l *localAPI) pauseWorker(w *worker, reason string) {
	w.mu.Lock()
	switch w.state {
	case workerStarting:
		w.pendingPause = reason
		w.mu.Unlock()
		return
	case workerRunning, workerPaused:
	default:
		w.mu.Unlock()
		return
	}
	first := w.state == workerRunning
	w.state = workerPaused
	s := w.sess
	w.mu.Unlock()
	s.setPaused(true, reason)
	if first && canPause {
		if err := w.signal((*procCtl).pause); err != nil {
			l.log.Warn("pause worker", "agent", w.agentID, "err", err)
		}
	}
	if first {
		l.log.Info("worker paused", "agent", w.agentID, "reason", reason)
	}
}

// resumeWorker continues a paused worker.
func (l *localAPI) resumeWorker(w *worker) {
	w.mu.Lock()
	switch {
	case w.state == workerStarting:
		w.pendingPause = ""
		w.mu.Unlock()
		return
	case w.state != workerPaused || w.stopping:
		w.mu.Unlock()
		return
	}
	w.state = workerRunning
	s := w.sess
	w.mu.Unlock()
	// A frozen node keeps the process stopped; Unfreeze continues it.
	if canPause && !l.n.isFrozen() {
		if err := w.signal((*procCtl).resume); err != nil {
			l.log.Warn("resume worker", "agent", w.agentID, "err", err)
		}
	}
	s.setPaused(false, "")
	l.log.Info("worker resumed", "agent", w.agentID)
}

// stopWorker terminates a worker's process group and kills it after the
// grace period. reason is reported with agent.exited. On a starting worker
// the stop is applied right after the process starts.
func (l *localAPI) stopWorker(w *worker, reason string) {
	w.mu.Lock()
	switch w.state {
	case workerStarting:
		if w.pendingStop == "" {
			w.pendingStop = reason
		}
		w.mu.Unlock()
		return
	case workerRunning, workerPaused:
	default:
		w.mu.Unlock()
		return
	}
	if w.stopping {
		w.mu.Unlock()
		return
	}
	w.stopping = true
	if w.stopReason == "" {
		w.stopReason = reason
	}
	s := w.sess
	w.mu.Unlock()
	s.setPaused(true, "stopping")
	l.log.Info("stopping worker", "agent", w.agentID, "reason", reason)
	if err := w.signal((*procCtl).terminate); err != nil {
		l.log.Debug("terminate worker", "agent", w.agentID, "err", err)
	}
	grace := l.n.cfg.workerKillGrace
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-w.done:
		case <-t.C:
			_ = w.signal((*procCtl).kill)
		}
	}()
}

// stopAllWorkers refuses new spawns, stops every worker and waits until
// their exits are reported (best effort, bounded).
func (l *localAPI) stopAllWorkers(reason string) {
	l.mu.Lock()
	l.closed = true
	var list []*worker
	for _, w := range l.workers {
		list = append(list, w)
	}
	l.mu.Unlock()
	for _, w := range list {
		l.stopWorker(w, reason)
	}
	deadline := time.NewTimer(l.n.cfg.workerKillGrace + usageReportFinal + exitReportTimeout + time.Second)
	defer deadline.Stop()
	for _, w := range list {
		w.mu.Lock()
		started := w.ctl != nil
		w.mu.Unlock()
		if !started {
			continue
		}
		select {
		case <-w.finished:
		case <-deadline.C:
			return
		}
	}
}

// workerOf returns the worker whose session is s.
func (l *localAPI) workerOf(s *agentSession) *worker {
	if !s.worker {
		return nil
	}
	l.mu.Lock()
	w := l.workers[s.name]
	l.mu.Unlock()
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sess != nil && w.sess != s {
		return nil
	}
	return w
}

// parseAgentID splits "agent:<name>@<node>".
func parseAgentID(id string) (name, node string, ok bool) {
	rest, ok := strings.CutPrefix(id, "agent:")
	if !ok {
		return "", "", false
	}
	name, node, ok = strings.Cut(rest, "@")
	return name, node, ok && identity.ValidNodeName(name) && node != ""
}

// agentCtl applies an AgentCtl from the hub to a local worker or session.
// It returns the error text for the CtlResponse, empty on success.
func (l *localAPI) agentCtl(c *mycelv1.AgentCtl) string {
	name, node, ok := parseAgentID(c.GetAgentId())
	if !ok || node != l.n.name() {
		return "invalid_argument: agent is not on this node"
	}
	action := c.GetAction()
	if action != "pause" && action != "resume" && action != "stop" {
		return "invalid_argument: unknown action " + strconv.Quote(action)
	}
	reason := c.GetReason()
	if reason == "" {
		reason = "paused by hub"
	}
	l.mu.Lock()
	w := l.workers[name]
	s := l.byName[name]
	l.mu.Unlock()
	l.log.Info("agent control from hub", "agent", c.GetAgentId(), "action", action, "reason", c.GetReason())
	// A worker that is starting or running is controlled as a worker, never
	// through its session. Only after it exited may a plain session that
	// later took the name be controlled.
	if w != nil && w.stateNow() != workerExited {
		switch action {
		case "pause":
			l.pauseWorker(w, reason)
		case "resume":
			l.resumeWorker(w)
		case "stop":
			l.stopWorker(w, exitStopped)
		}
		return ""
	}
	if s == nil || s.worker {
		return "not_found: no local agent " + c.GetAgentId()
	}
	switch action {
	case "pause":
		s.setPaused(true, reason)
	case "resume":
		s.setPaused(false, "")
	case "stop":
		l.endSession(s)
	}
	return ""
}

// ---- listing and DELETE ----

type workerUsage struct {
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
	Turns  int32   `json:"turns"`
}

type workerEntry struct {
	AgentID    string      `json:"agent_id"`
	Parent     string      `json:"parent"`
	Harness    string      `json:"harness"`
	Channel    string      `json:"channel"`
	State      string      `json:"state"`
	PID        int         `json:"pid"`
	StartedAt  *string     `json:"started_at"`
	ExitedAt   *string     `json:"exited_at"`
	ExitCode   *int        `json:"exit_code"`
	ExitReason string      `json:"exit_reason,omitempty"`
	Usage      workerUsage `json:"usage"`
}

func (w *worker) entry() workerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := workerEntry{
		AgentID: w.agentID, Parent: w.parent, Harness: w.harness, Channel: w.channel, State: w.state, PID: w.pid,
		Usage: workerUsage{Tokens: w.tokens, USD: w.usd, Turns: w.turns},
	}
	if !w.startedAt.IsZero() {
		s := w.startedAt.Format(time.RFC3339)
		e.StartedAt = &s
	}
	if w.state == workerExited {
		s := w.exitedAt.Format(time.RFC3339)
		code := w.exitCode
		e.ExitedAt, e.ExitCode, e.ExitReason = &s, &code, w.exitReason
	}
	return e
}

func (l *localAPI) handleWorkers(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	l.pruneWorkersLocked()
	list := make([]*worker, 0, len(l.workers))
	for _, wk := range l.workers {
		list = append(list, wk)
	}
	l.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	out := make([]workerEntry, 0, len(list))
	for _, wk := range list {
		out = append(out, wk.entry())
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

// sessionHeader optionally names the calling session on DELETE
// /api/local/workers/{agent}.
const sessionHeader = "Mycel-Session"

// handleStopWorker stops a worker given by agent id or name. The local
// token authorises it. A Mycel-Session header is advisory: when present,
// its session's agent must be the worker's parent (so an orchestrator that
// registered again can still stop its workers); any local token holder can
// leave the header out.
func (l *localAPI) handleStopWorker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("agent")
	if n, node, ok := parseAgentID(name); ok {
		if node != l.n.name() {
			writeJSONError(w, http.StatusNotFound, "unknown worker")
			return
		}
		name = n
	}
	l.mu.Lock()
	wk := l.workers[name]
	l.mu.Unlock()
	if wk == nil {
		writeJSONError(w, http.StatusNotFound, "unknown worker")
		return
	}
	if sid := r.Header.Get(sessionHeader); sid != "" {
		if s := l.session(sid); s == nil || s.agentID != wk.parent {
			writeJSONError(w, http.StatusForbidden, "only the spawning agent may stop this worker")
			return
		}
	}
	if wk.stateNow() == workerExited {
		writeJSONError(w, http.StatusConflict, "worker already exited")
		return
	}
	l.stopWorker(wk, exitStopped)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// runningWorkers counts workers that are running or paused.
func (l *localAPI) runningWorkers() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.workers {
		if st := w.stateNow(); st == workerRunning || st == workerPaused {
			n++
		}
	}
	return n
}
