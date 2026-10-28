//go:build fakeharness

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/test/fakeharness"
)

func TestStreamEmitter(t *testing.T) {
	var b strings.Builder
	steps := []fakeharness.Step{
		{Tool: "join_channel", Usage: &fakeharness.StepUsage{Input: 100, Output: 20, USD: 0.01}},
		{Tool: "wait_for_message", Repeat: 3},
		{Tool: "post", Usage: &fakeharness.StepUsage{Input: 50, Output: 5, USD: 0.002}},
	}
	em := &streamEmitter{w: &b, steps: steps, last: -1}
	for _, st := range []int{0, 1, 1, 1, 2} { // step 1 repeats: printed once
		em.observe(fakeharness.Call{Step: st})
	}
	if err := em.result(false); err != nil {
		t.Fatal(err)
	}
	want := `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":20}}}
{"type":"assistant","message":{"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":5}}}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.012,"usage":{"input_tokens":150,"output_tokens":25}}
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestMycelEnv(t *testing.T) {
	got := mycelEnv([]string{"PATH=/bin", "MYCEL_HOME=/h", "HOME=/u", "MYCEL_LOCAL_ADDR=127.0.0.1:9"})
	if want := []string{"MYCEL_HOME=/h", "MYCEL_LOCAL_ADDR=127.0.0.1:9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mycelEnv = %v, want %v", got, want)
	}
}

func TestFakeHarnessCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.json")
	if err := os.WriteFile(script, []byte(`[{"tool":"whoami","usage":{"input":7,"output":3,"usd":0.5}},{"tool":"post","args":{"channel":"#x","text":"done"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYCEL_HOME", dir)
	t.Setenv("MYCEL_LOCAL_ADDR", "127.0.0.1:7")

	type call struct {
		bin, agent, harness string
		env                 []string
		steps               int
	}
	var got call
	fail := false
	runFakeScript = func(_ context.Context, bin string, env []string, agent, harness string, steps []fakeharness.Step, observe func(fakeharness.Call)) (fakeharness.Transcript, error) {
		got = call{bin, agent, harness, env, len(steps)}
		tr := fakeharness.Transcript{}
		for i := range steps {
			c := fakeharness.Call{Index: i, Step: i, Tool: steps[i].Tool, Text: "ok"}
			tr.Calls = append(tr.Calls, c)
			observe(c)
			if fail {
				return tr, errors.New("step 0 (whoami): boom")
			}
		}
		return tr, nil
	}
	t.Cleanup(func() { runFakeScript = fakeharness.RunObserved })

	res := runCLI(t, "", "fakeharness", "--script", script, "--agent", "w1", "--prompt", "You are agent w1")
	if res.code != 0 {
		t.Fatalf("fakeharness: %+v", res)
	}
	self, _ := os.Executable()
	if got.bin != self || got.agent != "w1" || got.harness != "fake" || got.steps != 2 {
		t.Errorf("run = %+v", got)
	}
	if !reflect.DeepEqual(got.env, []string{"MYCEL_HOME=" + dir, "MYCEL_LOCAL_ADDR=127.0.0.1:7"}) {
		// Other MYCEL_* variables of the test environment may be present; require ours.
		joined := strings.Join(got.env, "\n")
		if !strings.Contains(joined, "MYCEL_HOME="+dir) || !strings.Contains(joined, "MYCEL_LOCAL_ADDR=127.0.0.1:7") {
			t.Errorf("env = %v", got.env)
		}
	}
	wantOut := `{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3}}}
{"type":"assistant","message":{"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":0.5,"usage":{"input_tokens":7,"output_tokens":3}}
`
	if res.stdout != wantOut {
		t.Errorf("stdout =\n%s\nwant\n%s", res.stdout, wantOut)
	}
	if !strings.Contains(res.stderr, "You are agent w1") {
		t.Errorf("prompt not logged: %q", res.stderr)
	}

	fail = true
	res = runCLI(t, "", "fakeharness", "--script", script, "--agent", "w1")
	if res.code != 1 || !strings.Contains(res.stderr, "boom") || !strings.Contains(res.stderr, "#0 step 0 whoami") {
		t.Errorf("failing script: %+v", res)
	}
	if !strings.HasSuffix(res.stdout, `{"type":"result","subtype":"error","is_error":true,"num_turns":1,"total_cost_usd":0.5,"usage":{"input_tokens":7,"output_tokens":3}}`+"\n") {
		t.Errorf("failing script stdout: %q", res.stdout)
	}

	for _, args := range [][]string{
		{"fakeharness", "--agent", "w1"},
		{"fakeharness", "--script", script},
		{"fakeharness", "--script", script, "--agent", "w1", "extra"},
	} {
		if res := runCLI(t, "", args...); res.code != 2 {
			t.Errorf("%v: code %d, want 2", args, res.code)
		}
	}
	if res := runCLI(t, "", "fakeharness", "--script", filepath.Join(dir, "none.json"), "--agent", "w1"); res.code != 1 {
		t.Errorf("missing script: %+v", res)
	}
	if res := runCLI(t, "", "--help"); strings.Contains(res.stdout, "fakeharness") {
		t.Error("fakeharness is listed in help")
	}
}
