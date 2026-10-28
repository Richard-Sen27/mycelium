package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/richard-sen27/mycelium/harnesses"
)

// Usage parsers a manifest's spawn section may name.
const (
	usageClaudeStreamJSON = "claude-stream-json"
	usageNone             = "none"
)

// testManifests, when set (tests only, before any daemon runs), is consulted
// before the embedded manifests.
var testManifests map[string][]byte

// readManifest returns the raw manifest for a harness name.
func readManifest(name string) ([]byte, error) {
	if b, ok := testManifests[name]; ok {
		return b, nil
	}
	return harnesses.Read(name)
}

// manifestNames lists every known harness name.
func manifestNames() []string {
	names := harnesses.Names()
	for n := range maps.Keys(testManifests) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// spawnSpec is the `spawn` section of a harness manifest (harnesses/README.md).
type spawnSpec struct {
	Command         string         `yaml:"command"`
	Args            []string       `yaml:"args"`
	MCPConfig       map[string]any `yaml:"mcp_config"`
	Usage           string         `yaml:"usage"`
	MaxTurnsDefault int            `yaml:"max_turns_default"`
	Prompt          string         `yaml:"prompt"`
}

type spawnManifest struct {
	Name  string     `yaml:"name"`
	Spawn *spawnSpec `yaml:"spawn"`
}

// errNoSpawn: the harness has no manifest or no usable spawn section.
var errNoSpawn = errors.New("harness cannot spawn workers")

// loadSpawnSpec returns the spawn section of harness name.
func loadSpawnSpec(name string) (*spawnSpec, error) {
	b, err := readManifest(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoSpawn
		}
		return nil, err
	}
	var m spawnManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("harness manifest %s: %w", name, err)
	}
	if m.Spawn == nil || m.Spawn.Command == "" {
		return nil, errNoSpawn
	}
	switch m.Spawn.Usage {
	case "":
		m.Spawn.Usage = usageNone
	case usageNone, usageClaudeStreamJSON:
	default:
		return nil, fmt.Errorf("harness manifest %s: unknown usage parser %q", name, m.Spawn.Usage)
	}
	return m.Spawn, nil
}

// spawnableHarnesses lists harnesses whose manifest has a spawn section.
func spawnableHarnesses() []string {
	var out []string
	for _, n := range manifestNames() {
		if _, err := loadSpawnSpec(n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// envPlaceholderPrefix: {{env:NAME}} may only name daemon environment
// variables with this prefix (docs/decisions.md D020), so a manifest cannot
// copy arbitrary secrets of the daemon into a worker's command line.
const envPlaceholderPrefix = "MYCEL_FAKE_"

// renderVars holds the placeholder values of one spawn.
type renderVars struct {
	vals map[string]string
}

// render replaces every {{name}} and {{env:MYCEL_FAKE_*}} in s in a single pass, so
// placeholder-like text inside a value (a task, say) is never expanded. An
// unknown placeholder is an error: it is a manifest bug.
func (v renderVars) render(s string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.Index(s[i+2:], "}}")
		if j < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		key := s[i+2 : i+2+j]
		b.WriteString(s[:i])
		if name, ok := strings.CutPrefix(key, "env:"); ok {
			if !strings.HasPrefix(name, envPlaceholderPrefix) || !validEnvName(name) {
				return "", fmt.Errorf("placeholder {{%s}}: only %s* variables may be used", key, envPlaceholderPrefix)
			}
			b.WriteString(os.Getenv(name))
		} else if val, ok := v.vals[key]; ok {
			b.WriteString(val)
		} else {
			return "", fmt.Errorf("unknown placeholder {{%s}}", key)
		}
		s = s[i+2+j+2:]
	}
}

// renderTree renders every string in a decoded YAML tree.
func (v renderVars) renderTree(x any) (any, error) {
	switch t := x.(type) {
	case string:
		return v.render(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			r, err := v.renderTree(e)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := v.renderTree(e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return x, nil
	}
}

// addLocalAddr appends --local-addr to every MCP server entry that runs
// `<mycel> mcp`, so the worker's MCP process finds this daemon on any port.
func addLocalAddr(cfg map[string]any, mycel, localAddr string) {
	servers, _ := cfg["mcpServers"].(map[string]any)
	for _, e := range servers {
		srv, ok := e.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := srv["command"].(string)
		args, _ := srv["args"].([]any)
		if cmd != mycel || len(args) == 0 || args[0] != "mcp" {
			continue
		}
		srv["args"] = append(args, "--local-addr", localAddr)
	}
}
