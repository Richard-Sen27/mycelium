package fakeharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EnvLocalAddr names the environment variable (in the env passed to Run)
// whose value is handed to `mycel mcp` as --local-addr.
const EnvLocalAddr = "MYCEL_LOCAL_ADDR"

// EnvTranscriptDir names the environment variable (in the env passed to Run)
// naming a directory: when set, a run writes its transcript there as
// <agent>.txt when it ends, so a test can inspect the transcripts of workers
// that a node daemon started.
const EnvTranscriptDir = "MYCEL_FAKE_TRANSCRIPT_DIR"

// RaceDetected is what the race detector prints on stderr.
const RaceDetected = "WARNING: DATA RACE"

// RaceExitCode is the exit code of a race-enabled binary that halts on its
// first data race (GORACE=halt_on_error=1, see ChildEnv).
const RaceExitCode = 66

// raceOptions is added to GORACE by ChildEnv.
const raceOptions = "halt_on_error=1"

// Step is one scripted tool call.
type Step struct {
	Tool        string         `json:"tool"`                   // MCP tool name; "" with SleepMS only pauses
	Args        map[string]any `json:"args,omitempty"`         // tool arguments; {{name}}, {{name|msgid}} and {{name|msgid:<substring>}} are substituted in strings
	Expect      string         `json:"expect,omitempty"`       // substring the result text must contain ("" = any)
	ExpectAll   []string       `json:"expect_all,omitempty"`   // each must appear in some result of this step (retried with Repeat); Expect applies to the last
	ExpectError bool           `json:"expect_error,omitempty"` // the step must end in a tool error whose text contains Expect (retried with Repeat)
	Store       string         `json:"store,omitempty"`        // store the result text under this name
	Repeat      int            `json:"repeat,omitempty"`       // run up to Repeat times until Expect matches
	AllowError  bool           `json:"allow_error,omitempty"`  // a tool error, substitution failure or Expect mismatch does not fail the script
	SleepMS     int            `json:"sleep_ms,omitempty"`     // with Tool "": pause this long (keeps the MCP session alive without consuming the inbox)
	Usage       *StepUsage     `json:"usage,omitempty"`        // usage the `mycel fakeharness` wrapper reports for this step (stream-json)
}

// StepUsage is the token and cost usage a step reports when the script runs
// as a spawned worker through `mycel fakeharness`.
type StepUsage struct {
	Input  int     `json:"input,omitempty"`  // input tokens
	Output int     `json:"output,omitempty"` // output tokens
	USD    float64 `json:"usd,omitempty"`    // cost in US dollars
}

// Call is one tool call made by the harness.
type Call struct {
	Index   int            `json:"index"` // position in the transcript
	Step    int            `json:"step"`  // index of the script step
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args,omitempty"` // after substitution
	Text    string         `json:"text"`           // concatenated text content (or the error)
	IsError bool           `json:"is_error"`
}

// Transcript records every call of a run.
type Transcript struct {
	Calls  []Call `json:"calls"`
	Stderr string `json:"stderr,omitempty"` // the adapter's stderr
}

// Contains reports whether any call's result text contains s.
func (t Transcript) Contains(s string) bool {
	for _, c := range t.Calls {
		if strings.Contains(c.Text, s) {
			return true
		}
	}
	return false
}

// String renders the transcript for test logs.
func (t Transcript) String() string {
	var b strings.Builder
	for _, c := range t.Calls {
		args, _ := json.Marshal(c.Args)
		mark := ""
		if c.IsError {
			mark = " (error)"
		}
		fmt.Fprintf(&b, "#%d step %d %s %s%s\n%s\n", c.Index, c.Step, c.Tool, args, mark, c.Text)
	}
	if t.Stderr != "" {
		fmt.Fprintf(&b, "---- adapter stderr ----\n%s", t.Stderr)
	}
	return b.String()
}

// LoadScript reads a JSON array of steps.
func LoadScript(path string) ([]Step, error) {
	b, err := os.ReadFile(path) //nolint:gosec // test script path
	if err != nil {
		return nil, err
	}
	var s []Step
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Run starts `<bin> mcp --agent <agent> --harness <harness>` with env and runs
// the script against it. See RunObserved.
func Run(ctx context.Context, bin string, env []string, agent, harness string, script []Step) (Transcript, error) {
	return RunObserved(ctx, bin, env, agent, harness, script, nil)
}

// RunObserved is Run with a callback invoked after every call, so a test can
// act while the script is still running. The child environment is
// ChildEnv(env...); env must hold MYCEL_HOME. When env sets MYCEL_LOCAL_ADDR
// it is passed as --local-addr; when it sets MYCEL_FAKE_TRANSCRIPT_DIR the
// transcript is written there at the end.
func RunObserved(ctx context.Context, bin string, env []string, agent, harness string, script []Step, observe func(Call)) (tr Transcript, err error) {
	args := []string{"mcp", "--agent", agent}
	if harness != "" {
		args = append(args, "--harness", harness)
	}
	if addr := lookupEnv(env, EnvLocalAddr); addr != "" {
		args = append(args, "--local-addr", addr)
	}
	if dir := lookupEnv(env, EnvTranscriptDir); dir != "" {
		// Registered first, so it runs after the session is closed and
		// the adapter's stderr is in the transcript.
		defer func() { writeTranscript(dir, agent, tr, err) }()
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // the binary under test
	cmd.Env = ChildEnv(env...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "fakeharness", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
	if err != nil {
		return tr, fmt.Errorf("fakeharness %s: connect: %w (stderr: %s)", agent, err, stderr.String())
	}
	defer func() {
		_ = cs.Close()
		tr.Stderr = stderr.String()
	}()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		return tr, fmt.Errorf("fakeharness %s: list tools: %w", agent, err)
	}
	tr, err = RunSession(ctx, cs, script, observe)
	if err != nil {
		return tr, fmt.Errorf("fakeharness %s: %w", agent, err)
	}
	return tr, nil
}

// writeTranscript writes the transcript of agent's run to <dir>/<agent>.txt,
// best effort.
func writeTranscript(dir, agent string, tr Transcript, runErr error) {
	text := tr.String()
	if runErr != nil {
		text += "---- run failed ----\n" + runErr.Error() + "\n"
	} else {
		text += "---- run succeeded ----\n"
	}
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, agent)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, name+".txt"), []byte(text), 0o600)
}

// RunSession runs script on an established MCP client session and returns
// the transcript. observe, if not nil, is called after every call.
func RunSession(ctx context.Context, cs *mcp.ClientSession, script []Step, observe func(Call)) (Transcript, error) {
	var tr Transcript
	stored := map[string]storedResult{}
	for i, st := range script {
		if err := runStep(ctx, cs, &tr, stored, i, st, observe); err != nil {
			return tr, fmt.Errorf("step %d (%s): %w", i, st.Tool, err)
		}
	}
	return tr, nil
}

// storedResult is a result text kept for {{name}} substitution, with the
// Expect of the step that stored it (used by {{name|msgid}}).
type storedResult struct {
	text   string
	expect string
}

func runStep(ctx context.Context, cs *mcp.ClientSession, tr *Transcript, stored map[string]storedResult, i int, st Step, observe func(Call)) error {
	if st.Tool == "" {
		if st.SleepMS <= 0 {
			return errors.New("step has no tool")
		}
		t := time.NewTimer(time.Duration(st.SleepMS) * time.Millisecond)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	record := func(c Call) {
		tr.Calls = append(tr.Calls, c)
		if observe != nil {
			observe(c)
		}
	}
	tries := max(st.Repeat, 1)
	seen := make([]bool, len(st.ExpectAll))
	var last Call
	for range tries {
		args, err := substitute(st.Args, stored)
		if err != nil {
			last = Call{Index: len(tr.Calls), Step: i, Tool: st.Tool, Text: "substitution: " + err.Error(), IsError: true}
			record(last)
			if st.AllowError {
				return nil
			}
			return err
		}
		last = call(ctx, cs, len(tr.Calls), i, st.Tool, args)
		record(last)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if st.Store != "" {
			stored[st.Store] = storedResult{text: last.Text, expect: st.Expect}
		}
		if st.ExpectError {
			if last.IsError && strings.Contains(last.Text, st.Expect) {
				return nil
			}
			continue
		}
		if last.IsError && !st.AllowError {
			return fmt.Errorf("tool error: %s", last.Text)
		}
		all := true
		for k, s := range st.ExpectAll {
			seen[k] = seen[k] || strings.Contains(last.Text, s)
			all = all && seen[k]
		}
		if all && strings.Contains(last.Text, st.Expect) {
			return nil
		}
	}
	if st.AllowError {
		return nil
	}
	if st.ExpectError {
		return fmt.Errorf("expected a tool error containing %q after %d tries, last result (error %t): %s", st.Expect, tries, last.IsError, last.Text)
	}
	var missing []string
	for k, s := range st.ExpectAll {
		if !seen[k] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("never saw %q in %d tries, last result: %s", missing, tries, last.Text)
	}
	return fmt.Errorf("expected %q in result after %d tries, last result: %s", st.Expect, tries, last.Text)
}

func call(ctx context.Context, cs *mcp.ClientSession, index, step int, tool string, args map[string]any) Call {
	c := Call{Index: index, Step: step, Tool: tool, Args: args}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		c.Text, c.IsError = err.Error(), true
		return c
	}
	var parts []string
	for _, ct := range res.Content {
		if t, ok := ct.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	c.Text, c.IsError = strings.Join(parts, "\n"), res.IsError
	return c
}

var (
	// {{name}}, {{name|msgid}} or {{name|msgid:<substring>}}.
	placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*(?:\|\s*([a-z]+)(?::([^}]*))?\s*)?\}\}`)
	msgIDRe       = regexp.MustCompile(`msg_[A-Za-z0-9]+`)
	// headerIDRe finds the id in a quoted message header.
	headerIDRe = regexp.MustCompile(`\[mycel message (msg_[A-Za-z0-9]+)`)
)

// substitute returns a deep copy of args with placeholders in every string
// replaced from stored.
func substitute(args map[string]any, stored map[string]storedResult) (map[string]any, error) {
	if args == nil {
		return nil, nil
	}
	v, err := substValue(args, stored)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

func substValue(v any, stored map[string]storedResult) (any, error) {
	switch x := v.(type) {
	case string:
		return substString(x, stored)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			s, err := substValue(e, stored)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			s, err := substValue(e, stored)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	case []string:
		out := make([]string, len(x))
		for i, e := range x {
			s, err := substString(e, stored)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	default:
		return v, nil
	}
}

func substString(s string, stored map[string]storedResult) (string, error) {
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		val, ok := stored[sub[1]]
		if !ok {
			fail(fmt.Errorf("no stored result %q", sub[1]))
			return m
		}
		switch sub[2] {
		case "":
			return val.text
		case "msgid":
			near := val.expect
			if sub[3] != "" {
				near = sub[3]
			}
			id := msgID(val.text, near)
			if id == "" {
				fail(fmt.Errorf("no msg_ id in stored result %q: %q", sub[1], val.text))
			}
			return id
		default:
			fail(fmt.Errorf("unknown filter %q", sub[2]))
			return m
		}
	})
	return out, firstErr
}

// msgID picks a message id from a result text. When near occurs in text,
// it is the id of the last quoted message header that starts at or before
// the first occurrence, so a wait result holding several messages yields
// the one that matched. Otherwise, and for results without headers such as
// "posted msg_…", it is the first msg_ id in the text.
func msgID(text, near string) string {
	if near != "" {
		if idx := strings.Index(text, near); idx >= 0 {
			id := ""
			for _, m := range headerIDRe.FindAllStringSubmatchIndex(text, -1) {
				if m[0] > idx {
					break
				}
				id = text[m[2]:m[3]]
			}
			if id != "" {
				return id
			}
		}
	}
	return msgIDRe.FindString(text)
}

func lookupEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// ChildEnv is the environment for a test's child process: the parent's
// environment without MYCEL_* variables, with halt_on_error=1 added to
// GORACE (a race-enabled child then exits with RaceExitCode on its first
// data race), plus extra.
func ChildEnv(extra ...string) []string {
	out := make([]string, 0, len(extra)+64)
	gorace := ""
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(strings.ToUpper(k), "MYCEL_"):
		case k == "GORACE":
			gorace = v
		default:
			out = append(out, kv)
		}
	}
	if !strings.Contains(gorace, raceOptions) {
		gorace = strings.TrimSpace(gorace + " " + raceOptions)
	}
	out = append(out, "GORACE="+gorace)
	return append(out, extra...)
}

// syncBuffer is a bytes.Buffer safe for the exec copier and readers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
