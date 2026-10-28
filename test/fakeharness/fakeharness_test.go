package fakeharness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSubstitute(t *testing.T) {
	stored := map[string]string{
		"task": "posted msg_01JABCDEF seq 3 in #task-x (thread none, hop 0)",
		"name": "worker",
	}
	got, err := substitute(map[string]any{
		"reply_to": "{{task|msgid}}",
		"text":     "hi {{ name }}",
		"mentions": []any{"agent:{{name}}@beta"},
		"nested":   map[string]any{"x": "{{name}}"},
		"limit":    3,
	}, stored)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"reply_to": "msg_01JABCDEF",
		"text":     "hi worker",
		"mentions": []any{"agent:worker@beta"},
		"nested":   map[string]any{"x": "worker"},
		"limit":    3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}

	for _, bad := range []string{"{{missing}}", "{{name|msgid}}", "{{name|upper}}"} {
		if _, err := substitute(map[string]any{"a": bad}, stored); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if got, err := substitute(nil, stored); err != nil || got != nil {
		t.Errorf("nil args: got %v, %v", got, err)
	}
}

func TestLoadScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	src := `[{"tool":"join_channel","args":{"channel":"#x"},"usage":{"input":120,"output":30,"usd":0.004}},
	{"tool":"wait_for_message","args":{"timeout_seconds":5},"expect":"hi","store":"m","repeat":3,"allow_error":true},
	{"sleep_ms":10}]`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScript(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 3 || s[1].Repeat != 3 || s[1].Store != "m" || !s[1].AllowError || s[1].Args["timeout_seconds"] != 5.0 || s[2].SleepMS != 10 {
		t.Fatalf("unexpected script: %+v", s)
	}
	if u := s[0].Usage; u == nil || *u != (StepUsage{Input: 120, Output: 30, USD: 0.004}) || s[1].Usage != nil {
		t.Fatalf("usage: %+v, %+v", s[0].Usage, s[1].Usage)
	}
	if _, err := LoadScript(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("missing file: want an error")
	}
}

func TestChildEnvAndLookup(t *testing.T) {
	t.Setenv("MYCEL_API_KEY", "secret")
	env := childEnv([]string{"MYCEL_HOME=/x", "MYCEL_LOCAL_ADDR=127.0.0.1:1"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "MYCEL_API_KEY=") {
			t.Fatal("parent MYCEL_API_KEY leaked into the child env")
		}
	}
	if lookupEnv(env, EnvLocalAddr) != "127.0.0.1:1" || lookupEnv(env, "MYCEL_HOME") != "/x" {
		t.Fatalf("lookupEnv: %v", env)
	}
}

func TestTranscript(t *testing.T) {
	tr := Transcript{Calls: []Call{{Tool: "post", Text: "hop limit (4) reached", IsError: true}}}
	if !tr.Contains("hop limit") || tr.Contains("nope") {
		t.Fatal("Contains")
	}
	if !strings.Contains(tr.String(), "(error)") {
		t.Fatalf("String: %s", tr.String())
	}
}
