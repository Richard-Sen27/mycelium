package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/richard-sen27/mycelium/harnesses"
)

// harnessManifest is harnesses/<name>.yaml (format: harnesses/README.md).
type harnessManifest struct {
	Name        string `yaml:"name"`
	DisplayName string `yaml:"display_name"`
	Install     string `yaml:"install"`
	Detect      string `yaml:"detect"`
	MCP         struct {
		File  string         `yaml:"file"`
		Entry map[string]any `yaml:"entry"`
		// Instructions replace File and Entry for harnesses without a
		// per-project MCP config file: setup prints them and writes nothing.
		Instructions string `yaml:"instructions"`
	} `yaml:"mcp"`
	Hooks *struct {
		File    string `yaml:"file"`
		Entries map[string][]struct {
			Command string `yaml:"command"`
		} `yaml:"entries"`
	} `yaml:"hooks"`
}

// agentNameRe keeps agent names safe to splice into a hook's shell command.
var agentNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func loadManifest(name, agent string) (*harnessManifest, error) {
	raw, err := harnesses.Read(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, usagef("unknown harness %q; known harnesses: %s", name, strings.Join(harnesses.Names(), ", "))
	}
	if err != nil {
		return nil, err
	}
	raw = bytes.ReplaceAll(raw, []byte("{{agent}}"), []byte(agent))
	var m harnessManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("harness manifest %s: %w", name, err)
	}
	if (m.MCP.File == "" || len(m.MCP.Entry) == 0) && strings.TrimSpace(m.MCP.Instructions) == "" {
		return nil, fmt.Errorf("harness manifest %s has no mcp.file and mcp.entry, nor mcp.instructions", name)
	}
	return &m, nil
}

type touchedFile struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	What    string `json:"what"`
}

func (a *app) agentCmd() *cobra.Command {
	cmd := groupCmd("agent", "Connect agent harnesses on this machine")
	cmd.AddCommand(a.agentSetupCmd(), a.agentSpawnCmd())
	return cmd
}

func (a *app) agentSetupCmd() *cobra.Command {
	var agent, dir string
	cmd := &cobra.Command{
		Use:   "setup <harness> --agent <name> [--dir .]",
		Short: "Write a harness's project config so it joins Mycelium as an agent",
		Long: "Writes the harness's MCP server entry (and hooks where supported) into the project directory.\n" +
			"Known harnesses: " + strings.Join(harnesses.Names(), ", "),
		Args: exactArgs(1, "harness"),
		RunE: func(_ *cobra.Command, args []string) error {
			if agent == "" {
				return usagef("--agent is required")
			}
			if !agentNameRe.MatchString(agent) {
				return usagef("invalid agent name %q: use lowercase letters, digits and '-' (at most 63)", agent)
			}
			m, err := loadManifest(args[0], agent)
			if err != nil {
				return err
			}
			if m.MCP.File == "" {
				return a.printSetupInstructions(m, agent)
			}
			var touched []touchedFile
			p := filepath.Join(dir, filepath.FromSlash(m.MCP.File))
			changed, err := mergeJSONFile(p, 0o644, func(doc map[string]any) error {
				servers, err := objectAt(doc, "mcpServers", p)
				if err != nil {
					return err
				}
				for k, v := range m.MCP.Entry {
					servers[k] = v
				}
				return nil
			})
			if err != nil {
				return err
			}
			touched = append(touched, touchedFile{Path: p, Changed: changed, What: "mcpServers"})
			if m.Hooks != nil && m.Hooks.File != "" && len(m.Hooks.Entries) > 0 {
				p := filepath.Join(dir, filepath.FromSlash(m.Hooks.File))
				changed, err := mergeJSONFile(p, 0o600, func(doc map[string]any) error {
					hooks, err := objectAt(doc, "hooks", p)
					if err != nil {
						return err
					}
					for event, entries := range m.Hooks.Entries {
						for _, e := range entries {
							setHookCommand(hooks, event, e.Command)
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
				touched = append(touched, touchedFile{Path: p, Changed: changed, What: "hooks"})
			}
			if a.jsonOut {
				return a.printJSONValue(map[string]any{"harness": m.Name, "agent": agent, "files": touched})
			}
			for _, t := range touched {
				state := "updated"
				if !t.Changed {
					state = "unchanged"
				}
				_, _ = fmt.Fprintf(a.stdout, "%s %s (%s)\n", state, t.Path, t.What)
			}
			display := m.DisplayName
			if display == "" {
				display = m.Name
			}
			next := "start " + display
			if f := strings.Fields(m.Detect); len(f) > 0 {
				next = "run `" + f[0] + "`"
			}
			_, err = fmt.Fprintf(a.stdout, "\n%s will join Mycelium as agent %q while `mycel daemon` runs on this machine.\nNext: %s in %s\n", display, agent, next, absOr(dir))
			return err
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent name (required)")
	cmd.Flags().StringVar(&dir, "dir", ".", "project directory")
	return cmd
}

// printSetupInstructions handles a harness whose MCP servers live only in its
// user-wide config: setup writes nothing and tells the user what to add.
func (a *app) printSetupInstructions(m *harnessManifest, agent string) error {
	text := strings.TrimRight(m.MCP.Instructions, "\n")
	if a.jsonOut {
		return a.printJSONValue(map[string]any{"harness": m.Name, "agent": agent, "files": []touchedFile{}, "instructions": text})
	}
	display := m.DisplayName
	if display == "" {
		display = m.Name
	}
	_, err := fmt.Fprintf(a.stdout, "%s has no per-project MCP config; nothing was written.\n\n%s\n\n%s will then join Mycelium as agent %q while `mycel daemon` runs on this machine.\n",
		display, text, display, agent)
	return err
}

func absOr(dir string) string {
	if p, err := filepath.Abs(dir); err == nil {
		return p
	}
	return dir
}

// objectAt returns doc[key] as an object, creating it when missing. A
// present value that is not an object is an error: setup never clobbers it.
func objectAt(doc map[string]any, key, path string) (map[string]any, error) {
	v, ok := doc[key]
	if !ok || v == nil {
		m := map[string]any{}
		doc[key] = m
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %q is not a JSON object; fix it and run setup again", path, key)
	}
	return m, nil
}

// hookFamily returns the part of a hook command that identifies it as ours
// regardless of the agent: its first two words ("mycel hook ").
func hookFamily(command string) string {
	f := strings.Fields(command)
	if len(f) < 2 {
		return command
	}
	return f[0] + " " + f[1] + " "
}

// setHookCommand makes command the only hook of its family under
// hooks[event] (Claude Code's format: [{"hooks":[{"type":"command","command":…}]}]).
// Hooks of the same family for another agent are replaced, others kept.
func setHookCommand(hooks map[string]any, event, command string) {
	family := hookFamily(command)
	groups, _ := hooks[event].([]any)
	present := false
	out := make([]any, 0, len(groups)+1)
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		inner, ok := gm["hooks"].([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		kept := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			switch {
			case !ok || !strings.HasPrefix(cmd+" ", family):
				kept = append(kept, h)
			case cmd == command && !present:
				present = true
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 && len(inner) > 0 {
			continue
		}
		if len(kept) != len(inner) {
			gm["hooks"] = kept
		}
		out = append(out, gm)
	}
	if !present {
		out = append(out, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		})
	}
	hooks[event] = out
}

// mergeJSONFile loads the JSON object at path (or starts empty), applies
// edit, and writes it back pretty-printed when it changed, keeping the
// existing file's mode (newMode for a new file). The write goes through a
// synced temporary file and a rename. Invalid JSON is an error, never
// overwritten.
func mergeJSONFile(path string, newMode os.FileMode, edit func(map[string]any) error) (bool, error) {
	doc := map[string]any{}
	mode := newMode
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
		if len(bytes.TrimSpace(old)) > 0 {
			if err := json.Unmarshal(old, &doc); err != nil {
				return false, fmt.Errorf("%s is not a JSON object (%v); fix or move it and run setup again", path, err)
			}
			if doc == nil {
				doc = map[string]any{}
			}
		}
	case errors.Is(err, os.ErrNotExist):
		old = nil
	default:
		return false, err
	}
	if err := edit(doc); err != nil {
		return false, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return false, err
	}
	if old != nil && jsonEqual(old, buf.Bytes()) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, buf.Bytes(), mode); err != nil {
		return false, err
	}
	return true, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".mycel-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
