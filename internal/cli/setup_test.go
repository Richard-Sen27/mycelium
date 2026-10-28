package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readJSONFile(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestAgentSetupFromScratch(t *testing.T) {
	dir := t.TempDir()
	res := runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir)
	if res.code != 0 {
		t.Fatalf("setup: %+v", res)
	}
	for _, w := range []string{"updated " + filepath.Join(dir, ".mcp.json"), "updated " + filepath.Join(dir, ".claude", "settings.json"), "Next: run `claude` in"} {
		if !strings.Contains(res.stdout, w) {
			t.Errorf("stdout missing %q:\n%s", w, res.stdout)
		}
	}
	mcpDoc := readJSONFile(t, filepath.Join(dir, ".mcp.json"))
	want := map[string]any{"mcpServers": map[string]any{"mycel": map[string]any{
		"command": "mycel", "args": []any{"mcp", "--agent", "coder", "--harness", "claude-code"},
	}}}
	if !reflect.DeepEqual(mcpDoc, want) {
		t.Errorf(".mcp.json = %v\nwant %v", mcpDoc, want)
	}
	settings := readJSONFile(t, filepath.Join(dir, ".claude", "settings.json"))
	wantS := map[string]any{"hooks": map[string]any{"UserPromptSubmit": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "mycel hook --agent coder"}}},
	}}}
	if !reflect.DeepEqual(settings, wantS) {
		t.Errorf("settings.json = %v\nwant %v", settings, wantS)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(raw), "\n  \"mcpServers\": {") {
		t.Errorf(".mcp.json is not pretty-printed:\n%s", raw)
	}
}

func TestAgentSetupMergesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"github":{"command":"gh-mcp"}},"other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{
  "model": "opus",
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "echo hi"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "notify"}]}]
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir)
	if res.code != 0 {
		t.Fatalf("setup: %+v", res)
	}
	mcpDoc := readJSONFile(t, filepath.Join(dir, ".mcp.json"))
	servers := mcpDoc["mcpServers"].(map[string]any)
	if _, ok := servers["github"]; !ok || servers["mycel"] == nil || mcpDoc["other"] != float64(1) {
		t.Errorf(".mcp.json lost entries: %v", mcpDoc)
	}
	settings := readJSONFile(t, filepath.Join(dir, ".claude", "settings.json"))
	hooks := settings["hooks"].(map[string]any)
	ups := hooks["UserPromptSubmit"].([]any)
	if settings["model"] != "opus" || hooks["Stop"] == nil || len(ups) != 2 {
		t.Fatalf("settings.json = %v", settings)
	}
	before1, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	before2, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))

	res = runCLI(t, "", "--json", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir)
	if res.code != 0 {
		t.Fatalf("second setup: %+v", res)
	}
	var out struct {
		Files []touchedFile `json:"files"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil || len(out.Files) != 2 {
		t.Fatalf("json output %q: %v", res.stdout, err)
	}
	for _, f := range out.Files {
		if f.Changed {
			t.Errorf("%s changed on the second run", f.Path)
		}
	}
	after1, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	after2, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if string(before1) != string(after1) || string(before2) != string(after2) {
		t.Error("second run rewrote the files")
	}
}

func TestAgentSetupErrors(t *testing.T) {
	dir := t.TempDir()
	res := runCLI(t, "", "agent", "setup", "nope", "--agent", "coder", "--dir", dir)
	if res.code != 2 || !strings.Contains(res.stderr, "known harnesses: claude-code") {
		t.Errorf("unknown harness: %+v", res)
	}
	res = runCLI(t, "", "agent", "setup", "claude-code", "--dir", dir)
	if res.code != 2 || !strings.Contains(res.stderr, "--agent is required") {
		t.Errorf("missing agent: %+v", res)
	}
	res = runCLI(t, "", "agent", "setup", "claude-code", "--agent", "x; rm -rf /", "--dir", dir)
	if res.code != 2 || !strings.Contains(res.stderr, "invalid agent name") {
		t.Errorf("bad agent: %+v", res)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	res = runCLI(t, "", "agent", "setup", "claude-code", "--agent", "coder", "--dir", dir)
	if res.code != 1 || !strings.Contains(res.stderr, "is not a JSON object") {
		t.Errorf("broken json: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".mcp.json")); string(b) != `{broken` {
		t.Errorf("broken file was overwritten: %s", b)
	}
}
