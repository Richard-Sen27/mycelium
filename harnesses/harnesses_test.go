package harnesses

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	placeholderRe = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
	// Only MYCEL_FAKE_* variables may be read from the daemon's environment.
	envRe = regexp.MustCompile(`^env:MYCEL_FAKE_[A-Z0-9_]+$`)
	// A documented placeholder: backticked on its own in README.md.
	readmePlaceholderRe = regexp.MustCompile("`\\{\\{([a-z_]+(?::[A-Z]+)?)\\}\\}`")
	setupPlaceholders   = []string{"agent"} // setup substitutes only the agent name
	usageParsers        = []string{"claude-stream-json", "none"}
)

// documented returns the placeholders README.md documents (without the
// {{env:NAME}} form) and whether it documents {{env:NAME}} with the
// MYCEL_FAKE_ prefix rule.
func documented(t *testing.T) (names []string, env bool) {
	t.Helper()
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range readmePlaceholderRe.FindAllStringSubmatch(string(b), -1) {
		switch {
		case m[1] == "env:NAME":
			env = true
		case !slices.Contains(names, m[1]):
			names = append(names, m[1])
		}
	}
	if env && !strings.Contains(string(b), "`MYCEL_FAKE_`") {
		t.Fatal("README.md documents {{env:NAME}} but not the MYCEL_FAKE_ prefix rule")
	}
	for _, need := range []string{"agent", "parent", "channel", "task", "harness", "prompt", "mycel"} {
		if !slices.Contains(names, need) {
			t.Fatalf("README.md does not document {{%s}} (found %v)", need, names)
		}
	}
	return names, env
}

func load(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := Read(name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// strings collects every string in v (keys included).
func collectStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case map[string]any:
		for k, e := range x {
			*out = append(*out, k)
			collectStrings(e, out)
		}
	case []any:
		for _, e := range x {
			collectStrings(e, out)
		}
	}
}

func checkPlaceholders(t *testing.T, where string, v any, allowed []string, allowEnv bool) {
	t.Helper()
	var ss []string
	collectStrings(v, &ss)
	for _, s := range ss {
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			p := m[1]
			if slices.Contains(allowed, p) || (allowEnv && envRe.MatchString(p)) {
				continue
			}
			t.Errorf("%s: placeholder {{%s}} is not documented here (allowed: %v)", where, p, allowed)
		}
		if rest := placeholderRe.ReplaceAllString(s, ""); strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
			t.Errorf("%s: malformed placeholder in %q", where, s)
		}
	}
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func TestManifests(t *testing.T) {
	names := Names()
	if want := []string{"claude-code", "codex", "fake", "gemini-cli"}; !reflect.DeepEqual(names, want) {
		t.Errorf("Names() = %v, want %v", names, want)
	}
	withSpawn := map[string]bool{"claude-code": true, "codex": true, "fake": true}
	spawnPlaceholders, envDocumented := documented(t)
	var promptPlaceholders []string // the prompt template cannot refer to itself
	for _, p := range spawnPlaceholders {
		if p != "prompt" {
			promptPlaceholders = append(promptPlaceholders, p)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			m := load(t, name)
			if str(m, "name") != name || !nameRe.MatchString(name) {
				t.Errorf("name = %q, want %q ([a-z0-9-])", str(m, "name"), name)
			}
			for _, k := range []string{"display_name", "install", "detect"} {
				if strings.TrimSpace(str(m, k)) == "" {
					t.Errorf("%s is empty", k)
				}
			}
			mcp, _ := m["mcp"].(map[string]any)
			entry, _ := mcp["entry"].(map[string]any)
			hasFile := str(mcp, "file") != "" || mcp["entry"] != nil
			hasInstr := strings.TrimSpace(str(mcp, "instructions")) != ""
			switch {
			case hasFile && hasInstr:
				t.Error("mcp.file/entry and mcp.instructions are mutually exclusive")
			case hasFile:
				if str(mcp, "file") == "" {
					t.Error("mcp.entry without mcp.file")
				}
				if _, ok := entry["mycel"].(map[string]any); !ok {
					t.Error("mcp.entry has no mycel server")
				}
			case !hasInstr:
				t.Error("mcp needs file and entry, or instructions")
			}
			checkPlaceholders(t, "mcp", mcp, setupPlaceholders, false)
			checkPlaceholders(t, "hooks", m["hooks"], setupPlaceholders, false)

			spawn, ok := m["spawn"].(map[string]any)
			if ok != withSpawn[name] {
				t.Fatalf("has spawn = %v, want %v", ok, withSpawn[name])
			}
			if !ok {
				return
			}
			if str(spawn, "command") == "" {
				t.Error("spawn.command is empty")
			}
			args, _ := spawn["args"].([]any)
			if len(args) == 0 {
				t.Error("spawn.args is empty")
			}
			for i, a := range args {
				if _, ok := a.(string); !ok {
					t.Errorf("spawn.args[%d] = %#v is not a string", i, a)
				}
			}
			if !slices.Contains(usageParsers, str(spawn, "usage")) {
				t.Errorf("spawn.usage = %q, want one of %v", str(spawn, "usage"), usageParsers)
			}
			if n, ok := spawn["max_turns_default"].(int); !ok || n <= 0 {
				t.Errorf("spawn.max_turns_default = %#v, want a positive int", spawn["max_turns_default"])
			}
			mc, _ := spawn["mcp_config"].(map[string]any)
			servers, _ := mc["mcpServers"].(map[string]any)
			if _, ok := servers["mycel"].(map[string]any); !ok {
				t.Error("spawn.mcp_config.mcpServers.mycel is missing")
			}
			prompt := str(spawn, "prompt")
			for _, p := range []string{"{{agent}}", "{{parent}}", "{{channel}}", "{{task}}", "untrusted"} {
				if !strings.Contains(prompt, p) {
					t.Errorf("spawn.prompt misses %s", p)
				}
			}
			checkPlaceholders(t, "spawn.prompt", prompt, promptPlaceholders, false)
			rest := map[string]any{}
			for k, v := range spawn {
				if k != "prompt" {
					rest[k] = v
				}
			}
			checkPlaceholders(t, "spawn", rest, spawnPlaceholders, envDocumented)
			var mcArgs []string
			collectStrings(servers["mycel"], &mcArgs)
			if !slices.Contains(mcArgs, "{{harness}}") {
				t.Errorf("spawn.mcp_config mycel args must pass --harness {{harness}}, got %v", mcArgs)
			}
			var all []string
			collectStrings(rest, &all)
			if !strings.Contains(strings.Join(all, " "), "{{prompt}}") {
				t.Error("spawn never passes {{prompt}} to the harness")
			}
		})
	}
}

// TestClaudeCodeSpawnMatchesReadme keeps claude-code.yaml's spawn section
// identical to the one documented in README.md.
func TestClaudeCodeSpawnMatchesReadme(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "```yaml\nspawn:")
	if !ok {
		t.Fatal("README.md has no ```yaml block starting with spawn:")
	}
	block, _, _ := strings.Cut(after, "```")
	var doc map[string]any
	if err := yaml.Unmarshal([]byte("spawn:"+block), &doc); err != nil {
		t.Fatal(err)
	}
	m := load(t, "claude-code")
	if !reflect.DeepEqual(m["spawn"], doc["spawn"]) {
		t.Errorf("claude-code.yaml spawn = %#v\nREADME spawn = %#v", m["spawn"], doc["spawn"])
	}
}

func TestFakeSpawn(t *testing.T) {
	spawn := load(t, "fake")["spawn"].(map[string]any)
	want := []any{"fakeharness", "--script", "{{env:MYCEL_FAKE_SCRIPT}}", "--prompt", "{{prompt}}", "--agent", "{{agent}}"}
	if str(spawn, "command") != "{{mycel}}" || !reflect.DeepEqual(spawn["args"], want) || str(spawn, "usage") != "claude-stream-json" {
		t.Errorf("fake spawn = %#v", spawn)
	}
	if str(spawn, "prompt") != str(load(t, "claude-code")["spawn"].(map[string]any), "prompt") {
		t.Error("fake prompt differs from claude-code's")
	}
}

func TestEnvAllowlist(t *testing.T) {
	for in, ok := range map[string]bool{
		"env:MYCEL_FAKE_SCRIPT": true, "env:MYCEL_FAKE_X1": true,
		"env:MYCEL_HOME": false, "env:HOME": false, "env:MYCEL_FAKE_": false, "env:mycel_fake_x": false,
	} {
		if envRe.MatchString(in) != ok {
			t.Errorf("envRe(%q) = %v, want %v", in, !ok, ok)
		}
	}
}

func TestRead(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", "claude-code.yaml", "nope"} {
		if _, err := Read(bad); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Read(%q) = %v, want fs.ErrNotExist", bad, err)
		}
	}
}
