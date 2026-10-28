package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// Usage accounting limits. A worker's stdout is untrusted: totals saturate
// instead of wrapping, and every report is clamped to these caps (the hub
// refuses larger reports).
const (
	// Per-report caps, identical to what the hub accepts (internal/hub
	// lifecycle.go); anything beyond them is carried over to the next report.
	maxReportTokens = 10_000_000
	maxReportUSD    = 1000.0
	maxReportTurns  = 10_000
	maxTotalUSD     = 1e12
	// maxSeenIDs bounds the assistant message ids remembered for dedupe.
	// Claude Code emits the events of one message back to back, so a
	// cleared set loses nothing in practice.
	maxSeenIDs = 4096
)

// satAdd64 adds non-negative b to a without overflowing.
func satAdd64(a, b int64) int64 {
	b = max(b, 0)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func satAdd32(a, b int32) int32 {
	b = max(b, 0)
	if a > math.MaxInt32-b {
		return math.MaxInt32
	}
	return a + b
}

type streamUsage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	// cache_read_input_tokens is deliberately not counted: cache reads are
	// re-reads of context already paid for.
}

// total is input + output + cache creation tokens, saturating.
func (u *streamUsage) total() int64 {
	return satAdd64(satAdd64(max(u.InputTokens, 0), u.OutputTokens), u.CacheCreation)
}

type streamLine struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string       `json:"id"`
		Usage *streamUsage `json:"usage"`
	} `json:"message"`
	TotalCostUSD *float64     `json:"total_cost_usd"`
	NumTurns     *int64       `json:"num_turns"`
	Usage        *streamUsage `json:"usage"`
}

// applyStreamLine accounts one claude stream-json line. Lines that do not
// parse (including numbers that overflow int64) are ignored.
func (w *worker) applyStreamLine(line []byte) {
	var sl streamLine
	if json.Unmarshal(line, &sl) != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch sl.Type {
	case "assistant":
		if sl.Message == nil || sl.Message.Usage == nil {
			return
		}
		// Claude Code emits one event per content block, each repeating the
		// message's id and usage: count a message once.
		if id := sl.Message.ID; id != "" {
			if _, dup := w.seen[id]; dup {
				return
			}
			if w.seen == nil || len(w.seen) >= maxSeenIDs {
				w.seen = map[string]struct{}{}
			}
			w.seen[id] = struct{}{}
		}
		w.tokens = satAdd64(w.tokens, sl.Message.Usage.total())
		w.turns = satAdd32(w.turns, 1)
	case "result":
		// The result carries the run's totals: they replace the running
		// sums when larger (a total never moves backwards).
		if c := sl.TotalCostUSD; c != nil && !math.IsNaN(*c) && !math.IsInf(*c, 0) && *c > w.usd {
			w.usd = min(*c, maxTotalUSD)
		}
		if sl.Usage != nil {
			if t := sl.Usage.total(); t > w.tokens {
				w.tokens = t
			}
		}
		if n := sl.NumTurns; n != nil && *n > int64(w.turns) {
			w.turns = int32(min(*n, math.MaxInt32)) // #nosec G115 -- clamped
		}
	}
}

// parseUsage reads the worker's stdout to EOF.
func (l *localAPI) parseUsage(w *worker, r io.Reader) {
	if w.parser != usageClaudeStreamJSON {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	scanLines(r, maxUsageLine, w.applyStreamLine)
}

// scanLines calls fn for every line of r up to max bytes; longer lines are
// skipped.
func scanLines(r io.Reader, max int, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skip := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skip {
			if len(line)+len(chunk) > max {
				skip, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !skip && len(line) > 0 {
			fn(line)
		}
		line, skip = line[:0], false
		if err != nil {
			return
		}
	}
}

// usageLoop reports usage increments every usageReportEvery while the
// worker runs.
func (l *localAPI) usageLoop(w *worker) {
	t := time.NewTicker(l.n.cfg.usageReportEvery)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-l.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(l.ctx, localRPCTimeout)
			l.reportUsage(ctx, w)
			cancel()
		}
	}
}

// reportUsage sends the usage not yet reported, each part clamped to the
// report caps; the excess is carried over to the next report. A transient
// failure keeps the delta for the next report; an invalid_argument refusal
// drops the part that was sent.
func (l *localAPI) reportUsage(ctx context.Context, w *worker) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	w.mu.Lock()
	dt, du, dn := w.tokens-w.repTokens, w.usd-w.repUSD, w.turns-w.repTurns
	w.mu.Unlock()
	if dt <= 0 && du <= 0 && dn <= 0 {
		return
	}
	sentT := min(max(dt, 0), int64(maxReportTokens))
	sentU := min(max(du, 0), maxReportUSD)
	sentN := min(max(dn, 0), int32(maxReportTurns))
	req := &mycelv1.AgentUsageRequest{
		AgentId: w.agentID,
		Tokens:  sentT,
		Usd:     sentU,
		Turns:   sentN,
	}
	resp := &mycelv1.AgentUsageResponse{}
	err := l.n.hubRPC(ctx, bus.RPCAgentUsage, req, resp)
	var he *hubError
	switch {
	case err == nil:
	case errors.As(err, &he) && errCode(he.msg) == "invalid_argument":
		l.log.Warn("hub refused a usage report; dropping it", "agent", w.agentID, "err", he.msg,
			"tokens", req.GetTokens(), "usd", req.GetUsd(), "turns", req.GetTurns())
	default:
		l.log.Warn("report worker usage; will retry", "agent", w.agentID, "err", err)
		return
	}
	w.mu.Lock()
	w.repTokens, w.repUSD, w.repTurns = w.repTokens+sentT, w.repUSD+sentU, w.repTurns+sentN
	w.mu.Unlock()
	if err == nil && resp.GetPaused() {
		l.pauseWorker(w, "paused by hub: budget exhausted")
	}
}

// hubPausedReason is the local pause reason for a registration reply that
// says the agent is paused.
func hubPausedReason(resp *mycelv1.RegisterAgentResponse) (string, bool) {
	if !strings.EqualFold(resp.GetState(), "paused") {
		return "", false
	}
	if r := resp.GetPausedReason(); r != "" {
		return "paused by hub: " + r, true
	}
	return "paused by hub", true
}
