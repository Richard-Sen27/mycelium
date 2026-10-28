package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/hub"
)

// fakeJobs implements JobService, ArtifactService and the HTTPS artifact
// endpoints for CLI tests; it shares the auth check of fakeHub.
type fakeJobs struct {
	mycelv1connect.UnimplementedJobServiceHandler
	mycelv1connect.UnimplementedArtifactServiceHandler
	hub *fakeHub

	mu       sync.Mutex
	blob     []byte // bytes served by GET /api/artifacts/{id}
	meta     *mycelv1.Artifact
	putQuery string
	putBody  []byte
	putLen   int64
	putAuth  string
	getAuth  string
	getPath  string
	// logsGate, when set, is waited on between the first and the second
	// StreamLogs chunk (follow only).
	logsGate chan struct{}
	// streamHook, when set, serves StreamLogs (call counts from 1);
	// jobHook, when set, answers GetJob.
	streamHook func(call int, r *mycelv1.StreamLogsRequest, s *connect.ServerStream[mycelv1.StreamLogsResponse]) error
	jobHook    func(id string) *mycelv1.Job
	logCalls   []*mycelv1.StreamLogsRequest
	getJobs    int
}

func newFakeJobs(h *fakeHub) *fakeJobs {
	return &fakeJobs{hub: h, blob: []byte("artifact bytes\n")}
}

func testJob(id string) *mycelv1.Job {
	return &mycelv1.Job{
		Id:    id,
		Spec:  &mycelv1.JobSpec{Profile: "dev", Command: []string{"go", "test", "./..."}, Name: "tests", Channel: "#task-1", Outputs: []string{"dist/*"}},
		State: mycelv1.JobState_JOB_STATE_SUCCEEDED, Node: "win", Backend: "wsl2-gvisor", SubmittedBy: "user:usr_1",
		Artifacts: []string{"art_a"}, CreatedAt: "2026-10-28T14:59:00Z", LogBytes: 2048,
		Stats: &mycelv1.JobStats{StartedAt: "2026-10-28T14:59:05Z", FinishedAt: "2026-10-28T14:59:50Z"},
	}
}

func (f *fakeJobs) RunJob(_ context.Context, r *connect.Request[mycelv1.RunJobRequest]) (*connect.Response[mycelv1.RunJobResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RunJobResponse{Job: &mycelv1.Job{Id: "job_01", Spec: r.Msg.GetSpec(), State: mycelv1.JobState_JOB_STATE_QUEUED}}), nil
}

func (f *fakeJobs) ListJobs(_ context.Context, r *connect.Request[mycelv1.ListJobsRequest]) (*connect.Response[mycelv1.ListJobsResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	running := testJob("job_02")
	running.State = mycelv1.JobState_JOB_STATE_RUNNING
	running.ExitCode = 0
	return connect.NewResponse(&mycelv1.ListJobsResponse{Jobs: []*mycelv1.Job{testJob("job_01"), running}}), nil
}

func (f *fakeJobs) GetJob(_ context.Context, r *connect.Request[mycelv1.GetJobRequest]) (*connect.Response[mycelv1.GetJobResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.jobHook
	f.getJobs++
	f.mu.Unlock()
	if hook != nil {
		return connect.NewResponse(&mycelv1.GetJobResponse{Job: hook(r.Msg.GetId())}), nil
	}
	if r.Msg.GetId() != "job_01" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("job "+r.Msg.GetId()))
	}
	return connect.NewResponse(&mycelv1.GetJobResponse{Job: testJob("job_01")}), nil
}

func (f *fakeJobs) CancelJob(_ context.Context, r *connect.Request[mycelv1.CancelJobRequest]) (*connect.Response[mycelv1.CancelJobResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.CancelJobResponse{Job: &mycelv1.Job{Id: r.Msg.GetId(), State: mycelv1.JobState_JOB_STATE_CANCELLED}}), nil
}

func (f *fakeJobs) StreamLogs(ctx context.Context, r *connect.Request[mycelv1.StreamLogsRequest], s *connect.ServerStream[mycelv1.StreamLogsResponse]) error {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return err
	}
	f.mu.Lock()
	f.logCalls = append(f.logCalls, r.Msg)
	call, hook := len(f.logCalls), f.streamHook
	f.mu.Unlock()
	if hook != nil {
		return hook(call, r.Msg, s)
	}
	if !r.Msg.GetFollow() {
		return s.Send(&mycelv1.StreamLogsResponse{Data: []byte("all output\x1b[2J\n"), Eof: true})
	}
	if err := s.Send(&mycelv1.StreamLogsResponse{Data: []byte("first line\n")}); err != nil {
		return err
	}
	f.mu.Lock()
	gate := f.logsGate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("first chunk was never printed"))
		}
	}
	// "✓" split across two chunks.
	check := []byte("✓")
	if err := s.Send(&mycelv1.StreamLogsResponse{Data: append([]byte("second "), check[:1]...), Offset: 11}); err != nil {
		return err
	}
	return s.Send(&mycelv1.StreamLogsResponse{Data: append(check[1:], '\n'), Offset: 19, Eof: true})
}

func (f *fakeJobs) ListProfiles(_ context.Context, r *connect.Request[mycelv1.ListProfilesRequest]) (*connect.Response[mycelv1.ListProfilesResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListProfilesResponse{Profiles: []*mycelv1.Profile{
		{Name: "default", Version: 1, Backends: []string{"applevf", "gvisor"}, Limits: &mycelv1.Limits{Cpus: 2, MemoryBytes: 4 << 30, WallClock: "1h"}, Description: "no network"},
		{Name: "dev", Version: 3, Backends: []string{"applevf"}, Egress: &mycelv1.EgressPolicy{Allow: []string{"pypi.org", "*.github.com"}}, Brokers: []string{"docker-nested"}},
	}}), nil
}

func (f *fakeJobs) ListArtifacts(_ context.Context, r *connect.Request[mycelv1.ListArtifactsRequest]) (*connect.Response[mycelv1.ListArtifactsResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListArtifactsResponse{Artifacts: []*mycelv1.Artifact{
		{Id: "art_a", Name: "report.txt", Size: 2048, Sha256: "0123456789abcdef0123", JobId: "job_01", Channel: "#task-1", CreatedBy: "hub", CreatedAt: "2026-10-28T14:59:50Z"},
	}}), nil
}

func (f *fakeJobs) GetArtifact(_ context.Context, r *connect.Request[mycelv1.GetArtifactRequest]) (*connect.Response[mycelv1.GetArtifactResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meta != nil {
		return connect.NewResponse(&mycelv1.GetArtifactResponse{Artifact: f.meta}), nil
	}
	sum := sha256.Sum256(f.blob)
	return connect.NewResponse(&mycelv1.GetArtifactResponse{Artifact: &mycelv1.Artifact{
		Id: r.Msg.GetId(), Name: "out/report.txt", Size: int64(len(f.blob)), Sha256: hex.EncodeToString(sum[:]),
	}}), nil
}

func (f *fakeJobs) DeleteArtifact(_ context.Context, r *connect.Request[mycelv1.DeleteArtifactRequest]) (*connect.Response[mycelv1.DeleteArtifactResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.DeleteArtifactResponse{}), nil
}

func (f *fakeJobs) putArtifact(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.putQuery, f.putBody, f.putLen, f.putAuth = r.URL.RawQuery, b, r.ContentLength, r.Header.Get("Authorization")
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
		return
	}
	sum := sha256.Sum256(b)
	out, _ := protojson.Marshal(&mycelv1.Artifact{Id: "art_new", Name: r.URL.Query().Get("name"), Size: int64(len(b)), Sha256: hex.EncodeToString(sum[:]), Channel: r.URL.Query().Get("channel")})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out)
}

func (f *fakeJobs) getArtifactBytes(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.getAuth, f.getPath = r.Header.Get("Authorization"), r.URL.Path
	blob := f.blob
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
		return
	}
	if r.PathValue("id") == "art_gone" {
		http.Error(w, `{"error":"artifact art_gone"}`, http.StatusNotFound)
		return
	}
	_, _ = w.Write(blob)
}

func TestParseGitSource(t *testing.T) {
	tests := []struct{ in, url, ref string }{
		{"https://github.com/org/repo", "https://github.com/org/repo", ""},
		{"https://github.com/org/repo.git@main", "https://github.com/org/repo.git", "main"},
		{"https://github.com/org/repo@feature/x", "https://github.com/org/repo", "feature/x"},
		{"https://user@host.example/org/repo", "https://user@host.example/org/repo", ""},
		{"https://user@host.example/org/repo@v1.2.0", "https://user@host.example/org/repo", "v1.2.0"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", ""},
		{"git@github.com:org/repo.git@0a1b2c3", "git@github.com:org/repo.git", "0a1b2c3"},
		{"ssh://git@host/org/repo", "ssh://git@host/org/repo", ""},
		{"https://host", "https://host", ""},
	}
	for _, tc := range tests {
		g, err := parseGitSource(tc.in)
		if err != nil || g.GetUrl() != tc.url || g.GetRef() != tc.ref {
			t.Errorf("parseGitSource(%q) = %v, %v; want %q @ %q", tc.in, g, err, tc.url, tc.ref)
		}
	}
	if _, err := parseGitSource("https://h/r@"); err == nil {
		t.Error("empty ref accepted")
	}
}

func TestJobRunArguments(t *testing.T) {
	git := func(u, ref string) *mycelv1.WorkspaceSeed {
		return &mycelv1.WorkspaceSeed{Source: &mycelv1.WorkspaceSeed_Git{Git: &mycelv1.GitSource{Url: u, Ref: ref}}}
	}
	tests := []struct {
		name    string
		args    []string
		want    *mycelv1.JobSpec
		wantErr string // usage error (exit 2)
	}{
		{
			name: "minimal with --",
			args: []string{"--profile", "dev", "--", "go", "test", "-race", "./..."},
			want: &mycelv1.JobSpec{Profile: "dev", Command: []string{"go", "test", "-race", "./..."}},
		},
		{
			name: "command without -- keeps its own flags",
			args: []string{"--profile", "dev", "python", "train.py", "--epochs", "3"},
			want: &mycelv1.JobSpec{Profile: "dev", Command: []string{"python", "train.py", "--epochs", "3"}},
		},
		{
			name: "every flag",
			args: []string{"--profile", "trainer", "--node", "gpu-box", "--channel", "task-9", "--timeout", "30m", "--name", "train",
				"--input", "art_01=data/train.csv", "--input", "art_02", "--output", "model/*.pt", "--output", "logs/*",
				"--git", "https://github.com/org/ml.git@v2", "--env", "EPOCHS=3", "--env", "URL=https://x/?a=b",
				"--", "python", "train.py"},
			want: &mycelv1.JobSpec{
				Profile: "trainer", Node: "gpu-box", Channel: "#task-9", Timeout: "30m", Name: "train",
				Command:   []string{"python", "train.py"},
				Inputs:    []*mycelv1.JobInput{{Artifact: "art_01", Path: "data/train.csv"}, {Artifact: "art_02"}},
				Outputs:   []string{"model/*.pt", "logs/*"},
				Workspace: git("https://github.com/org/ml.git", "v2"),
				Env:       map[string]string{"EPOCHS": "3", "URL": "https://x/?a=b"},
			},
		},
		{
			name: "scp-style git without ref",
			args: []string{"--profile", "dev", "--git", "git@github.com:org/repo.git", "--", "make"},
			want: &mycelv1.JobSpec{Profile: "dev", Command: []string{"make"}, Workspace: git("git@github.com:org/repo.git", "")},
		},
		{
			name: "workspace artifact and empty env value",
			args: []string{"--profile", "dev", "--workspace", "art_ws", "--env", "EMPTY=", "--", "make"},
			want: &mycelv1.JobSpec{Profile: "dev", Command: []string{"make"}, Env: map[string]string{"EMPTY": ""},
				Workspace: &mycelv1.WorkspaceSeed{Source: &mycelv1.WorkspaceSeed_Artifact{Artifact: "art_ws"}}},
		},
		{name: "no profile", args: []string{"--", "make"}, wantErr: "--profile is required"},
		{name: "no command", args: []string{"--profile", "dev"}, wantErr: "give the command after --"},
		{name: "no command after --", args: []string{"--profile", "dev", "--"}, wantErr: "give the command after --"},
		{name: "workspace and git", args: []string{"--profile", "dev", "--workspace", "art_1", "--git", "https://h/r", "--", "x"}, wantErr: "cannot be combined"},
		{name: "workspace not an artifact", args: []string{"--profile", "dev", "--workspace", "./src", "--", "x"}, wantErr: "want an artifact id"},
		{name: "input not an artifact", args: []string{"--profile", "dev", "--input", "data.csv", "--", "x"}, wantErr: "want art_<id>[=path]"},
		{name: "env without =", args: []string{"--profile", "dev", "--env", "FOO", "--", "x"}, wantErr: "want KEY=VALUE"},
		{name: "env MYCEL_ reserved", args: []string{"--profile", "dev", "--env", "mycel_home=/", "--", "x"}, wantErr: "reserved"},
		{name: "env key starting with a digit", args: []string{"--profile", "dev", "--env", "1A=x", "--", "x"}, wantErr: "want KEY=VALUE"},
		{name: "env key with a dash", args: []string{"--profile", "dev", "--env", "A-B=x", "--", "x"}, wantErr: "want KEY=VALUE"},
		{name: "env key with a space", args: []string{"--profile", "dev", "--env", "A B=x", "--", "x"}, wantErr: "want KEY=VALUE"},
		{name: "env value with NUL", args: []string{"--profile", "dev", "--env", "A=b\x00c", "--", "x"}, wantErr: "NUL byte"},
		{name: "env twice", args: []string{"--profile", "dev", "--env", "A=1", "--env", "A=2", "--", "x"}, wantErr: "given twice"},
		{name: "bad timeout", args: []string{"--profile", "dev", "--timeout", "forever", "--", "x"}, wantErr: "invalid --timeout"},
		{name: "empty git ref", args: []string{"--profile", "dev", "--git", "https://h/r@", "--", "x"}, wantErr: "empty ref"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.writeConfig(t)
			res := runCLI(t, "", append([]string{"job", "run"}, tc.args...)...)
			if tc.wantErr != "" {
				if res.code != 2 || !strings.Contains(res.stderr, tc.wantErr) {
					t.Fatalf("got %+v, want usage error %q", res, tc.wantErr)
				}
				if env.hub.lastRequest() != nil {
					t.Error("a request was sent")
				}
				return
			}
			if res.code != 0 {
				t.Fatalf("job run: %+v", res)
			}
			got, ok := env.hub.lastRequest().(*mycelv1.RunJobRequest)
			if !ok || !proto.Equal(got.GetSpec(), tc.want) {
				t.Errorf("spec = %v\nwant %v", got.GetSpec(), tc.want)
			}
			if !strings.Contains(res.stdout, "Job job_01 queued (profile "+tc.want.GetProfile()+").") {
				t.Errorf("stdout = %q", res.stdout)
			}
		})
	}
}

func TestJobAndArtifactCommands(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantCode  int
		wantOut   []string
		notOut    []string
		wantErr   []string
		checkReq  func(t *testing.T, m proto.Message)
		checkJSON bool
	}{
		{
			name: "job ls", args: []string{"job", "ls"},
			wantOut: []string{"ID", "PROFILE", "STATE", "EXIT", "job_01", "succeeded", "win", "1m ago", "job_02", "running"},
			checkReq: func(t *testing.T, m proto.Message) {
				if m.(*mycelv1.ListJobsRequest).GetIncludeFinished() {
					t.Error("include_finished without --all")
				}
			},
		},
		{
			name: "job ls --all json", args: []string{"--json", "job", "ls", "--all"}, checkJSON: true,
			wantOut: []string{`"jobs":[`, `"id":"job_01"`, `"state":"JOB_STATE_SUCCEEDED"`},
			checkReq: func(t *testing.T, m proto.Message) {
				if !m.(*mycelv1.ListJobsRequest).GetIncludeFinished() {
					t.Error("--all not sent")
				}
			},
		},
		{
			name: "job get", args: []string{"job", "get", "job_01"},
			wantOut: []string{"State:", "succeeded", "Command:", "go test ./...", "Node:", "win", "Backend:", "wsl2-gvisor", "Exit code:", "0", "Artifacts:", "art_a", "Output:", "2.0 KiB"},
		},
		{name: "job get unknown", args: []string{"job", "get", "job_x"}, wantCode: 1, wantErr: []string{"not found: job job_x"}},
		{
			name: "job cancel", args: []string{"job", "cancel", "job_01"}, wantOut: []string{"Job job_01 cancelled."},
			checkReq: func(t *testing.T, m proto.Message) {
				if m.(*mycelv1.CancelJobRequest).GetId() != "job_01" {
					t.Errorf("request = %v", m)
				}
			},
		},
		{
			name: "job logs without follow strips controls", args: []string{"job", "logs", "job_01"},
			wantOut: []string{"all output[2J\n"}, notOut: []string{"\x1b"},
			checkReq: func(t *testing.T, m proto.Message) {
				if r := m.(*mycelv1.StreamLogsRequest); r.GetFollow() || r.GetId() != "job_01" {
					t.Errorf("request = %v", r)
				}
			},
		},
		{
			name: "profiles ls", args: []string{"profiles", "ls"},
			wantOut: []string{"NAME", "BACKENDS", "EGRESS", "default", "applevf,gvisor", "4.0 GiB", "1h", "none", "no network", "dev", "pypi.org,*.github.com", "docker-nested"},
		},
		{
			name: "artifacts ls", args: []string{"artifacts", "ls", "--job", "job_01", "--channel", "task-1"},
			wantOut: []string{"ID", "SHA256", "art_a", "report.txt", "2.0 KiB", "0123456789ab", "job_01", "#task-1"},
			notOut:  []string{"0123456789abc"},
			checkReq: func(t *testing.T, m proto.Message) {
				if r := m.(*mycelv1.ListArtifactsRequest); r.GetJobId() != "job_01" || r.GetChannel() != "#task-1" {
					t.Errorf("request = %v", r)
				}
			},
		},
		{name: "artifacts rm", args: []string{"artifacts", "rm", "art_a"}, wantOut: []string{"Deleted art_a."}},
		{name: "job without subcommand", args: []string{"job"}, wantOut: []string{"run", "logs", "cancel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.writeConfig(t)
			res := runCLI(t, "", tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(res.stdout, s) {
					t.Errorf("stdout missing %q:\n%s", s, res.stdout)
				}
			}
			for _, s := range tt.notOut {
				if strings.Contains(res.stdout, s) {
					t.Errorf("stdout contains %q:\n%s", s, res.stdout)
				}
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(res.stderr, s) {
					t.Errorf("stderr missing %q:\n%s", s, res.stderr)
				}
			}
			if tt.checkJSON && !json.Valid([]byte(res.stdout)) {
				t.Errorf("stdout is not JSON: %s", res.stdout)
			}
			if tt.checkReq != nil {
				tt.checkReq(t, env.hub.lastRequest())
			}
		})
	}
}

// notifyWriter is a goroutine-safe buffer that signals every write.
type notifyWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func (w *notifyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestJobLogsFollowPrintsLiveChunks(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	gate := make(chan struct{})
	env.jobs.logsGate = gate
	out := &notifyWriter{wrote: make(chan struct{}, 1)}
	ta := newTestApp(t, "")
	ta.stdout = out
	done := make(chan result, 1)
	go func() { done <- ta.run("job", "logs", "job_01", "--follow") }()

	// The first chunk must be on stdout while the server still holds the rest.
	select {
	case <-out.wrote:
	case r := <-done:
		t.Fatalf("returned before streaming: %+v", r)
	case <-time.After(10 * time.Second):
		t.Fatal("first chunk not printed")
	}
	if got := out.String(); got != "first line\n" {
		t.Fatalf("after the first chunk stdout = %q", got)
	}
	close(gate)
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("logs --follow did not end at eof")
	}
	if r.code != 0 {
		t.Fatalf("logs --follow: %+v", r)
	}
	if got := out.String(); got != "first line\nsecond ✓\n" {
		t.Errorf("stdout = %q", got)
	}
	if req := env.hub.lastRequest().(*mycelv1.StreamLogsRequest); !req.GetFollow() {
		t.Error("follow not sent")
	}
}

func TestArtifactsPut(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "build.tar")
	content := bytes.Repeat([]byte("0123456789"), 100_000) // 1 MB
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "artifacts", "put", p, "--channel", "task-1")
	if res.code != 0 || !strings.Contains(res.stdout, "Uploaded build.tar as art_new (976.6 KiB, sha256 ") {
		t.Fatalf("put: %+v", res)
	}
	f := env.jobs
	f.mu.Lock()
	if !bytes.Equal(f.putBody, content) || f.putLen != int64(len(content)) || f.putQuery != "channel=%23task-1&name=build.tar" || f.putAuth != "Bearer "+testAPIKey {
		t.Errorf("put request: query %q len %d body ok %v auth %q", f.putQuery, f.putLen, bytes.Equal(f.putBody, content), f.putAuth)
	}
	f.mu.Unlock()

	res = runCLI(t, "", "--json", "artifacts", "put", p, "--name", "release.tar")
	if res.code != 0 || !strings.Contains(res.stdout, `"id":"art_new"`) || !strings.Contains(res.stdout, `"name":"release.tar"`) {
		t.Fatalf("put --json: %+v", res)
	}

	res = runCLI(t, "", "--api-key", "mck_wrong", "artifacts", "put", p)
	if res.code != 1 || !strings.Contains(res.stderr, "authentication failed: invalid api key") {
		t.Errorf("bad key: %+v", res)
	}
	res = runCLI(t, "", "artifacts", "put", dir)
	if res.code != 1 || !strings.Contains(res.stderr, "not a regular file") {
		t.Errorf("directory: %+v", res)
	}
}

func TestArtifactsGet(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	wd := t.TempDir()
	t.Chdir(wd)

	// Default destination: the artifact's base name in the current directory.
	res := runCLI(t, "", "artifacts", "get", "art_a")
	if res.code != 0 || !strings.Contains(res.stdout, "Saved art_a to report.txt (15 B).") {
		t.Fatalf("get: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(wd, "report.txt")); string(b) != "artifact bytes\n" {
		t.Errorf("file = %q", b)
	}
	env.jobs.mu.Lock()
	if env.jobs.getPath != "/api/artifacts/art_a" || env.jobs.getAuth != "Bearer "+testAPIKey {
		t.Errorf("GET %s auth %q", env.jobs.getPath, env.jobs.getAuth)
	}
	env.jobs.mu.Unlock()

	// It never overwrites a file it named itself.
	res = runCLI(t, "", "artifacts", "get", "art_a")
	if res.code != 1 || !strings.Contains(res.stderr, "report.txt already exists") {
		t.Errorf("existing: %+v", res)
	}

	// -o writes (and replaces) the given path.
	out := filepath.Join(wd, "copy.txt")
	res = runCLI(t, "", "--json", "artifacts", "get", "art_a", "-o", out)
	if res.code != 0 || !strings.Contains(res.stdout, `"path":"`+jsonEscape(out)+`"`) || !strings.Contains(res.stdout, `"size":15`) {
		t.Fatalf("get -o: %+v", res)
	}
	if b, _ := os.ReadFile(out); string(b) != "artifact bytes\n" {
		t.Errorf("-o file = %q", b)
	}

	// -o - writes to stdout.
	res = runCLI(t, "", "artifacts", "get", "art_a", "-o", "-")
	if res.code != 0 || res.stdout != "artifact bytes\n" {
		t.Errorf("get -o -: %+v", res)
	}

	// A sha256 mismatch fails and leaves nothing behind.
	env.jobs.mu.Lock()
	env.jobs.meta = &mycelv1.Artifact{Id: "art_a", Name: "bad.txt", Size: 15, Sha256: strings.Repeat("0", 64)}
	env.jobs.mu.Unlock()
	res = runCLI(t, "", "artifacts", "get", "art_a")
	if res.code != 1 || !strings.Contains(res.stderr, "does not match its sha256") {
		t.Errorf("mismatch: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wd, "bad.txt")); err == nil {
		t.Error("kept a corrupt download")
	}

	// A hostile name cannot leave the current directory.
	env.jobs.mu.Lock()
	env.jobs.meta = &mycelv1.Artifact{Id: "art_a", Name: "../../etc/evil"}
	env.jobs.mu.Unlock()
	res = runCLI(t, "", "artifacts", "get", "art_a")
	if res.code != 0 || !strings.Contains(res.stdout, "to evil ") {
		t.Errorf("hostile name: %+v", res)
	}
	env.jobs.mu.Lock()
	env.jobs.meta = &mycelv1.Artifact{Id: "art_a", Name: ".."}
	env.jobs.mu.Unlock()
	if res = runCLI(t, "", "artifacts", "get", "art_a"); res.code != 1 || !strings.Contains(res.stderr, "choose one with -o") {
		t.Errorf("name ..: %+v", res)
	}

	// The bytes endpoint's 404.
	env.jobs.mu.Lock()
	env.jobs.meta = nil
	env.jobs.mu.Unlock()
	if res = runCLI(t, "", "artifacts", "get", "art_gone", "-o", "gone"); res.code != 1 || !strings.Contains(res.stderr, "not found: artifact art_gone") {
		t.Errorf("404: %+v", res)
	}
	entries, _ := os.ReadDir(wd)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "copy.txt,evil,report.txt" {
		t.Errorf("files left: %v", names)
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

func TestArtifactFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.txt": "report.txt", "out/report.txt": "report.txt", `..\..\x.bin`: "x.bin",
		"..": "", ".": "", "": "", "dir/": "", "a\nb": "ab", "C:evil": "Cevil",
		`a<b>c"d|e?f*g.txt`: "abcdefg.txt", "report.txt. . ": "report.txt", "...": "",
		".bashrc": "_bashrc", ".ssh/authorized_keys": "authorized_keys", "..hidden": "_.hidden",
		"CON": "", "con": "", "nul.txt": "", "COM1": "", "com9.log": "", "LPT1.tar.gz": "", "AUX ": "",
		"PRN.": "", "COM¹": "", "CONOUT$": "", "COM10": "COM10", "CONSOLE": "CONSOLE", "lpt": "lpt", "x.con": "x.con",
	} {
		if got := artifactFileName(in); got != want {
			t.Errorf("artifactFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDaemonAllowFakeProfile(t *testing.T) {
	for _, allow := range []bool{false, true} {
		env := newTestEnv(t)
		initRoles(t, env.home, true, false)
		ta := newTestApp(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		ta.baseCtx = ctx
		var got hub.Options
		ta.hubRun = func(c context.Context, o hub.Options, r chan<- hub.Ready) error {
			got = o
			r <- hub.Ready{APIAddr: "127.0.0.1:1", BusAddr: "127.0.0.1:2"}
			cancel()
			<-c.Done()
			return c.Err()
		}
		args := []string{"daemon"}
		if allow {
			args = append(args, "--allow-fake-profile")
		}
		res := ta.run(args...)
		cancel()
		if res.code != 0 {
			t.Fatalf("daemon %v: %+v", args, res)
		}
		if got.AllowFakeProfile != allow {
			t.Errorf("daemon %v: AllowFakeProfile = %v", args, got.AllowFakeProfile)
		}
		if allow && !strings.Contains(res.stderr, "fake sandbox profile") {
			t.Errorf("no warning: %s", res.stderr)
		}
	}
}

func fastLogRetry(t *testing.T) {
	t.Helper()
	old := logRetryBase
	logRetryBase = time.Millisecond
	t.Cleanup(func() { logRetryBase = old })
}

type logStream = connect.ServerStream[mycelv1.StreamLogsResponse]

func sendLog(s *logStream, off uint64, data string, eof bool) error {
	return s.Send(&mycelv1.StreamLogsResponse{Offset: off, Data: []byte(data), Eof: eof})
}

func TestJobLogsFollowReconnects(t *testing.T) {
	unavailable := connect.NewError(connect.CodeUnavailable, errors.New("hub restarting"))
	running := func(id string) *mycelv1.Job {
		return &mycelv1.Job{Id: id, State: mycelv1.JobState_JOB_STATE_RUNNING}
	}
	tests := []struct {
		name      string
		stream    func(call int, r *mycelv1.StreamLogsRequest, s *logStream) error
		job       func(id string) *mycelv1.Job
		wantCode  int
		wantOut   string
		wantErr   string
		wantFroms []uint64 // from_byte of every StreamLogs call
	}{
		{
			name: "clean close, error, overlap and gap",
			stream: func(call int, _ *mycelv1.StreamLogsRequest, s *logStream) error {
				switch call {
				case 1: // closes cleanly mid-job, without eof
					return sendLog(s, 0, "first line\n", false)
				case 2: // resends 6 bytes already printed, then fails
					if err := sendLog(s, 5, " line\nsecond\n", false); err != nil {
						return err
					}
					return unavailable
				default: // 7 bytes were lost
					return sendLog(s, 25, "third\n", true)
				}
			},
			job:       running,
			wantOut:   "first line\nsecond\n\n[gap: 7 bytes of output missing]\nthird\n",
			wantFroms: []uint64{0, 11, 18},
		},
		{
			name: "stops when the job is finished and everything printed",
			stream: func(_ int, _ *mycelv1.StreamLogsRequest, s *logStream) error {
				return sendLog(s, 0, "done\n", false)
			},
			job: func(id string) *mycelv1.Job {
				return &mycelv1.Job{Id: id, State: mycelv1.JobState_JOB_STATE_SUCCEEDED, LogBytes: 5}
			},
			wantOut:   "done\n",
			wantFroms: []uint64{0},
		},
		{
			name: "finished with output left: redials for the rest",
			stream: func(call int, _ *mycelv1.StreamLogsRequest, s *logStream) error {
				if call == 1 {
					return sendLog(s, 0, "part ", false)
				}
				return sendLog(s, 5, "rest\n", true)
			},
			job: func(id string) *mycelv1.Job {
				return &mycelv1.Job{Id: id, State: mycelv1.JobState_JOB_STATE_FAILED, LogBytes: 10}
			},
			wantOut:   "part rest\n",
			wantFroms: []uint64{0, 5},
		},
		{
			name: "a refusal is final",
			stream: func(int, *mycelv1.StreamLogsRequest, *logStream) error {
				return connect.NewError(connect.CodeNotFound, errors.New("job job_x"))
			},
			wantCode: 1, wantErr: "not found: job job_x", wantFroms: []uint64{0},
		},
		{
			name: "GetJob refusal after a drop is final",
			stream: func(int, *mycelv1.StreamLogsRequest, *logStream) error {
				return unavailable
			},
			// no job hook: the default GetJob answers not found for job_x
			wantCode: 1, wantErr: "not found: job job_x", wantFroms: []uint64{0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fastLogRetry(t)
			env := newTestEnv(t)
			env.writeConfig(t)
			env.jobs.streamHook = tc.stream
			env.jobs.jobHook = tc.job
			res := runCLI(t, "", "job", "logs", "job_x", "--follow")
			if res.code != tc.wantCode || res.stdout != tc.wantOut || !strings.Contains(res.stderr, tc.wantErr) {
				t.Fatalf("got %+v\nwant code %d stdout %q stderr %q", res, tc.wantCode, tc.wantOut, tc.wantErr)
			}
			env.jobs.mu.Lock()
			defer env.jobs.mu.Unlock()
			var froms []uint64
			for _, r := range env.jobs.logCalls {
				if !r.GetFollow() || r.GetId() != "job_x" {
					t.Errorf("call %v", r)
				}
				froms = append(froms, r.GetFromByte())
			}
			if !slices.Equal(froms, tc.wantFroms) {
				t.Errorf("from_byte per call = %v, want %v", froms, tc.wantFroms)
			}
		})
	}
}

func TestJobLogsFollowStopsOnCancel(t *testing.T) {
	fastLogRetry(t)
	env := newTestEnv(t)
	env.writeConfig(t)
	env.jobs.streamHook = func(int, *mycelv1.StreamLogsRequest, *logStream) error {
		return connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	env.jobs.jobHook = func(id string) *mycelv1.Job { return &mycelv1.Job{Id: id, State: mycelv1.JobState_JOB_STATE_RUNNING} }
	ta := newTestApp(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ta.baseCtx = ctx
	res := ta.run("job", "logs", "job_x", "--follow")
	if res.code != 0 {
		t.Fatalf("logs --follow after cancel: %+v", res)
	}
	env.jobs.mu.Lock()
	defer env.jobs.mu.Unlock()
	if len(env.jobs.logCalls) < 2 {
		t.Errorf("only %d attempts: no reconnect", len(env.jobs.logCalls))
	}
}

func TestRetryableStreamError(t *testing.T) {
	if logRetryMax != 10*time.Second {
		t.Errorf("logRetryMax = %v, want 10s", logRetryMax)
	}
	for _, c := range []connect.Code{connect.CodeNotFound, connect.CodePermissionDenied, connect.CodeUnauthenticated, connect.CodeInvalidArgument} {
		if retryableStreamError(connect.NewError(c, errors.New("x"))) {
			t.Errorf("%v is retried", c)
		}
	}
	if !retryableStreamError(io.ErrUnexpectedEOF) || !retryableStreamError(connect.NewError(connect.CodeUnavailable, errors.New("x"))) {
		t.Error("transient errors are not retried")
	}
}

func TestArtifactEndpointsRejectUnpinnedCert(t *testing.T) {
	env := newTestEnv(t)
	d, _ := config.Resolve()
	if err := config.Save(d, &config.File{HubURL: env.srv.URL, APIKey: testAPIKey, CACertPEM: newCAPEM(t)}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, "", "artifacts", "put", p)
	if res.code != 1 || !strings.Contains(res.stderr, "certificate") {
		t.Fatalf("put to an unpinned server: %+v", res)
	}
	env.jobs.mu.Lock()
	defer env.jobs.mu.Unlock()
	if env.jobs.putBody != nil || env.jobs.putAuth != "" {
		t.Error("the file or the API key reached a server with an unpinned certificate")
	}
}
