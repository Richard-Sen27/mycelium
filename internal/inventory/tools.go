package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// cmdTimeout bounds every external command Probe runs. A variable so tests
// can lower it.
var cmdTimeout = 3 * time.Second

const ollamaVersionURL = "http://127.0.0.1:11434/api/version"

// runCmd runs name with args under a timeout derived from ctx and returns its
// stdout. Stdout is returned even when the command exits non-zero, since some
// tools print what we need and then fail on something unrelated.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...) //nolint:gosec // fixed command: callers pass fixed binaries and args
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	return stdout.Bytes(), err
}

// firstLine returns the first non-empty trimmed line of b.
func firstLine(b []byte) string {
	for l := range strings.Lines(string(b)) {
		if s := strings.TrimSpace(l); s != "" {
			return s
		}
	}
	return ""
}

func probeDocker(ctx context.Context) *mycelv1.ToolPresence {
	tp := &mycelv1.ToolPresence{}
	p, err := exec.LookPath("docker")
	if err != nil {
		return tp
	}
	tp.Present, tp.Path = true, p
	// Client version only, so a stopped daemon does not matter.
	out, _ := runCmd(ctx, p, "version", "--format", "{{.Client.Version}}")
	tp.Version = firstLine(out)
	return tp
}

func probeOllama(ctx context.Context) *mycelv1.ToolPresence {
	tp := &mycelv1.ToolPresence{}
	if p, err := exec.LookPath("ollama"); err == nil {
		tp.Present, tp.Path = true, p
	}
	if v, ok := ollamaHTTPVersion(ctx); ok {
		tp.Present = true
		if v != "" {
			tp.Version = v
			return tp
		}
	}
	if tp.Path != "" {
		out, _ := runCmd(ctx, tp.Path, "--version")
		tp.Version = parseOllamaVersion(string(out))
	}
	return tp
}

// ollamaHTTPVersion asks a local Ollama server for its version. ok reports
// whether a server answered at all.
func ollamaHTTPVersion(ctx context.Context) (v string, ok bool) {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, ollamaVersionURL, nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", true
	}
	return body.Version, true
}

var ollamaVersionRe = regexp.MustCompile(`version is (\S+)`)

// parseOllamaVersion extracts the version from `ollama --version` output, e.g.
// "ollama version is 0.5.7" or "Warning: client version is 0.5.7".
func parseOllamaVersion(out string) string {
	m := ollamaVersionRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}
