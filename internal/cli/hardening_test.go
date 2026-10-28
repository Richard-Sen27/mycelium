package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTermText(t *testing.T) {
	in := "ok\x1b[31mred\x07\tcol\nline\u009b2J"
	if got, want := termText(in), "ok[31mred\tcol\nline2J"; got != want {
		t.Errorf("termText = %q, want %q", got, want)
	}
	if got, want := cellText(in), "ok[31mred col line2J"; got != want {
		t.Errorf("cellText = %q, want %q", got, want)
	}
}

func TestReadStripsControls(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	env.ch.extraText = "evil\x1b]0;pwned\x07\r\u009bline\tok\nnext"
	for _, args := range [][]string{{"read", "#backend"}, {"read", "#backend", "--quote"}} {
		res := runCLI(t, "", args...)
		if res.code != 0 {
			t.Fatalf("read: %+v", res)
		}
		for _, r := range res.stdout {
			if r != '\n' && r != '\t' && isControl(r) {
				t.Fatalf("%v: control %U in output %q", args, r, res.stdout)
			}
		}
		if !strings.Contains(res.stdout, "evil") || !strings.Contains(res.stdout, "next") {
			t.Errorf("%v: text missing: %q", args, res.stdout)
		}
	}
	res := runCLI(t, "", "read", "#backend", "--json")
	if !strings.Contains(res.stdout, `\u001b]0;pwned`) {
		t.Errorf("--json must stay raw: %q", res.stdout)
	}
}

func TestSecretsNeverPrinted(t *testing.T) {
	const key = "mck_supersecretleak"
	if s := fmt.Sprintf("%v %+v %#v", bearerInterceptor(key), bearerInterceptor(key), bearerInterceptor(key)); strings.Contains(s, key) {
		t.Errorf("bearer formats the key: %s", s)
	}
	env := newTestEnv(t)
	env.writeConfig(t)
	res := runCLI(t, "", "-v", "--api-key", key, "channels", "ls")
	if res.code == 0 || strings.Contains(res.stdout+res.stderr, key) {
		t.Errorf("api key leaked or call succeeded: %+v", res)
	}

	const tok = "local-token-supersecret"
	writeLocalToken(t, tok)
	_, srv := newLocalFake(t, "another-token")
	for _, args := range [][]string{
		{"-v", "hook", "--agent", "coder", "--local-addr", srv.URL},
		{"-v", "agents", "ls", "--local", "--local-addr", srv.URL},
		{"-v", "mcp", "--agent", "coder", "--local-addr", srv.URL},
	} {
		res := runCLI(t, "", args...)
		if strings.Contains(res.stdout+res.stderr, tok) {
			t.Errorf("%v leaked the local token: %+v", args, res)
		}
		if !strings.Contains(res.stderr, "refused the sign-in token") {
			t.Errorf("%v: want a token error, got %+v", args, res)
		}
	}
}

func TestAgentSetupReplacesOldAgentHook(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{"hooks":{"UserPromptSubmit":[
		{"hooks":[{"type":"command","command":"mycel hook --agent old"},{"type":"command","command":"echo keep"}]},
		{"hooks":[{"type":"command","command":"mycel hook --agent older"}]}
	]}}`)
	if err := os.WriteFile(settings, existing, 0o640); err != nil { //nolint:gosec // a mode other than the default, to see it kept
		t.Fatal(err)
	}
	if res := runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir); res.code != 0 {
		t.Fatalf("setup: %+v", res)
	}
	doc := readJSONFile(t, settings)
	b, _ := json.Marshal(doc)
	got := string(b)
	if strings.Contains(got, "--agent old") || strings.Count(got, "mycel hook --agent coder") != 1 || !strings.Contains(got, "echo keep") {
		t.Errorf("settings = %s", got)
	}
	ups := doc["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(ups) != 2 {
		t.Errorf("want the kept group plus ours, got %d groups: %s", len(ups), got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(settings)
		if err != nil || st.Mode().Perm() != 0o640 {
			t.Errorf("mode = %v, %v; want the existing 0640 kept", st.Mode().Perm(), err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(settings))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mycel-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestAgentSetupNewFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := t.TempDir()
	if res := runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir); res.code != 0 {
		t.Fatalf("setup: %+v", res)
	}
	for p, want := range map[string]os.FileMode{".mcp.json": 0o644, ".claude/settings.json": 0o600} {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %v", p, st.Mode().Perm(), err, want)
		}
	}
}

func TestAgentSetupRefusesNonObject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":["x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir)
	if res.code != 1 || !strings.Contains(res.stderr, `"mcpServers" is not a JSON object`) {
		t.Errorf("setup: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".mcp.json")); string(b) != `{"mcpServers":["x"]}` {
		t.Errorf("file was rewritten: %s", b)
	}
}
