package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/richard-sen27/mycelium/internal/daemon"
)

// DefaultLocalURL is the node daemon's local API when no hub role runs on the
// same machine; with a hub role it is http://127.0.0.1:7779.
const DefaultLocalURL = "http://127.0.0.1:7777"

// ErrDaemonNotRunning is returned by ReadLocalToken when the node daemon has
// not written its local sign-in token.
var ErrDaemonNotRunning = errors.New("the mycel node daemon is not running on this machine")

// ReadLocalToken reads <nodeDir>/local.token.
func ReadLocalToken(nodeDir string) (string, error) {
	p := filepath.Join(nodeDir, daemon.LocalTokenFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w (no %s): start it with `mycel daemon`", ErrDaemonNotRunning, p)
	}
	if err != nil {
		return "", fmt.Errorf("read local token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%w (%s is empty): start it with `mycel daemon`", ErrDaemonNotRunning, p)
	}
	return tok, nil
}

// APIError is a non-2xx answer from the local API; Message is the server's
// "error" text.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "local API refused the sign-in token (" + e.Message + "); was the daemon restarted with a new token?"
	}
	return e.Message
}

// Session is a registered local agent.
type Session struct {
	AgentID string `json:"agent_id"`
	Session string `json:"session"`
}

// LocalAgent is one entry of GET /api/local/agents.
type LocalAgent struct {
	AgentID string `json:"agent_id"`
	Session string `json:"session"`
	Harness string `json:"harness"`
	Since   string `json:"since"`
}

// Name returns the agent name parsed from "agent:<name>@<node>".
func (a LocalAgent) Name() string { return AgentName(a.AgentID) }

// AgentName returns <name> from "agent:<name>@<node>" (or the input when it
// does not have that shape).
func AgentName(id string) string {
	s := strings.TrimPrefix(id, "agent:")
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Client calls the node daemon's local API (internal/daemon/localapi.go).
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for baseURL (e.g. http://127.0.0.1:7777),
// which must be a loopback http URL (see CheckLocalURL).
func NewClient(baseURL, token string) (*Client, error) {
	u, err := CheckLocalURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:  u,
		token: token,
		// No client timeout: inbox.pull long-polls; every call carries a
		// context deadline instead.
		http: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}},
	}, nil
}

// CheckLocalURL accepts "host:port" or an http URL whose host is 127.0.0.1,
// ::1 or localhost, and returns it as "http://host:port" without a trailing
// slash. The local sign-in token must never leave the machine.
func CheckLocalURL(s string) (string, error) {
	raw := strings.TrimSpace(s)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid local API address %q: want 127.0.0.1:<port>", s)
	}
	if u.Scheme != "http" {
		return "", fmt.Errorf("invalid local API address %q: the local API is plain http on loopback", s)
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
	default:
		return "", fmt.Errorf("invalid local API address %q: the host must be 127.0.0.1, ::1 or localhost", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("invalid local API address %q: want only scheme, host and port", s)
	}
	return "http://" + u.Host, nil
}

// String redacts the token.
func (c *Client) String() string { return "mcp.Client{" + c.base + ", token [redacted]}" }

// GoString redacts the token.
func (c *Client) GoString() string { return c.String() }

// URL returns the base URL.
func (c *Client) URL() string { return c.base }

var (
	marshal   = protojson.MarshalOptions{UseProtoNames: true}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// Register registers (or re-attaches to) the agent's session.
func (c *Client) Register(ctx context.Context, name, harness string) (*Session, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "harness": harness})
	var s Session
	if err := c.do(ctx, http.MethodPost, "/api/local/agents", body, func(b []byte) error { return json.Unmarshal(b, &s) }); err != nil {
		return nil, err
	}
	if s.Session == "" {
		return nil, errors.New("local API returned no session")
	}
	return &s, nil
}

// Agents lists the local agent sessions.
func (c *Client) Agents(ctx context.Context) ([]LocalAgent, error) {
	var out struct {
		Agents []LocalAgent `json:"agents"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/local/agents", nil, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// Delete stops a session.
func (c *Client) Delete(ctx context.Context, session string) error {
	return c.do(ctx, http.MethodDelete, "/api/local/agents/"+url.PathEscape(session), nil, nil)
}

// SessionHeader carries the caller's session on requests that act for a
// session without naming it in the path (DELETE /api/local/workers/{agent}),
// so the daemon can tell the spawning session from the local UI or CLI.
const SessionHeader = "Mycel-Session"

// DefaultProfile is the only worker profile in phase 3: a harness started on
// this machine, natively, under the user's own login.
const DefaultProfile = "trusted-host"

// SpawnBudget is a worker's budget; zero means unlimited.
type SpawnBudget struct {
	Tokens int64   `json:"tokens,omitempty"`
	USD    float64 `json:"usd,omitempty"`
}

// SpawnRequest is the body of POST /api/local/agents/{session}/spawn.
type SpawnRequest struct {
	Harness  string       `json:"harness"`
	Name     string       `json:"name,omitempty"`
	Task     string       `json:"task"`
	Channel  string       `json:"channel"`
	Profile  string       `json:"profile"`
	Workdir  string       `json:"workdir,omitempty"`
	MaxTurns int          `json:"max_turns,omitempty"`
	Budget   *SpawnBudget `json:"budget,omitempty"`
}

// MaxTaskBytes caps a spawn request's task text.
const MaxTaskBytes = 16 << 10

// CheckTask rejects an empty task or one longer than MaxTaskBytes.
func CheckTask(task string) error {
	if strings.TrimSpace(task) == "" {
		return errors.New("task is required")
	}
	if len(task) > MaxTaskBytes {
		return fmt.Errorf("task is %d bytes, more than the %d (16 KiB) allowed; put long material on the channel's blackboard and refer to it", len(task), MaxTaskBytes)
	}
	return nil
}

// SpawnResult is the reply of a spawn.
type SpawnResult struct {
	AgentID string `json:"agent_id"`
	Worker  string `json:"worker"`
}

// WorkerUsage is what a worker has used so far.
type WorkerUsage struct {
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
	Turns  int     `json:"turns"`
}

// Worker is one entry of GET /api/local/workers.
type Worker struct {
	AgentID   string      `json:"agent_id"`
	Parent    string      `json:"parent"`
	Harness   string      `json:"harness"`
	State     string      `json:"state"`
	PID       int         `json:"pid"`
	StartedAt string      `json:"started_at"`
	ExitedAt  string      `json:"exited_at"`
	ExitCode  int         `json:"exit_code"`
	Usage     WorkerUsage `json:"usage"`
}

// Spawn starts a local worker for session (spawn_agent with target self).
// An empty Profile becomes DefaultProfile.
func (c *Client) Spawn(ctx context.Context, session string, req SpawnRequest) (*SpawnResult, error) {
	if req.Profile == "" {
		req.Profile = DefaultProfile
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out SpawnResult
	path := "/api/local/agents/" + url.PathEscape(session) + "/spawn"
	if err := c.do(ctx, http.MethodPost, path, body, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	if out.AgentID == "" {
		out.AgentID = out.Worker
	}
	if out.AgentID == "" {
		return nil, errors.New("local API returned no worker id")
	}
	return &out, nil
}

// Workers lists the workers of this node.
func (c *Client) Workers(ctx context.Context) ([]Worker, error) {
	var out struct {
		Workers []Worker `json:"workers"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/local/workers", nil, func(b []byte) error { return json.Unmarshal(b, &out) }); err != nil {
		return nil, err
	}
	return out.Workers, nil
}

// StopWorker stops a worker. session is the caller's session, sent in
// SessionHeader; "" acts with the local sign-in token alone (CLI, local UI).
func (c *Client) StopWorker(ctx context.Context, session, agentID string) error {
	var h http.Header
	if session != "" {
		h = http.Header{SessionHeader: []string{session}}
	}
	return c.doHeader(ctx, http.MethodDelete, "/api/local/workers/"+url.PathEscape(agentID), nil, h, nil)
}

// Call invokes an agent RPC (a bus.RPCAgent* method) for session. req is the
// method's request without its `call` field; resp receives the reply.
func (c *Client) Call(ctx context.Context, session, method string, req, resp proto.Message) error {
	body, err := marshal.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	path := "/api/local/agents/" + url.PathEscape(session) + "/rpc/" + url.PathEscape(method)
	return c.do(ctx, http.MethodPost, path, body, func(b []byte) error {
		if resp == nil || len(bytes.TrimSpace(b)) == 0 {
			return nil
		}
		return unmarshal.Unmarshal(b, resp)
	})
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, decode func([]byte) error) error {
	return c.doHeader(ctx, method, path, body, nil, decode)
}

func (c *Client) doHeader(ctx context.Context, method, path string, body []byte, header http.Header, decode func([]byte) error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("local API at %s: %w", c.base, ctx.Err())
		}
		return fmt.Errorf("mycel node daemon not reachable at %s: %w", c.base, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read local API reply: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := ""
		if json.Unmarshal(b, &e) == nil {
			msg = e.Error
		}
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		if msg == "" {
			msg = res.Status
		}
		return &APIError{Status: res.StatusCode, Message: msg}
	}
	if decode == nil {
		return nil
	}
	if err := decode(b); err != nil {
		return fmt.Errorf("decode local API reply: %w", err)
	}
	return nil
}
