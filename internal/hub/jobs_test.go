package hub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// jobEnv is a hub with users alice, bob and carol, sandbox nodes owned by
// bob (fake backend, sandbox enabled; "w1" unless told otherwise) and node
// n0 (alice's, sandbox not enabled, fake backend) with alice's agent ag.
type jobEnv struct {
	h                            *testHub
	srv                          *server
	aliceID, bobID, carolID      string
	aliceKey, bobKey, carolKey   string
	adminKey                     string
	alice, bob, carol, adminJobs mycelv1connect.JobServiceClient
	aliceArt, bobArt, carolArt   mycelv1connect.ArtifactServiceClient
	aliceCh                      mycelv1connect.ChannelServiceClient
	aliceAgents                  mycelv1connect.AgentServiceClient
	adminClients, anonClients    clients
	bobClients                   clients
	nodes                        map[string]*enrolled
	sn                           map[string]*standIn
	w1, n0                       *enrolled
	sw1, sn0                     *standIn
	ag                           *testAgent
	aliceHTTP, nodeW1HTTP        *http.Client
	base                         string
}

// withJobConfig makes the hubs started by this test use cfg. Call it before
// starting the hub, so the old value comes back only after the hub stopped.
func withJobConfig(t *testing.T, cfg jobConfig) {
	t.Helper()
	old := newJobConfig
	newJobConfig = cfg
	t.Cleanup(func() { newJobConfig = old })
}

func newJobEnv(t *testing.T, allowFake bool) *jobEnv {
	t.Helper()
	return newJobEnvWith(t, allowFake, newJobConfig, "w1")
}

func newJobEnvWith(t *testing.T, allowFake bool, cfg jobConfig, sandboxNodes ...string) *jobEnv {
	t.Helper()
	withJobConfig(t, cfg)
	srvCh := make(chan *server, 1)
	testHookServer = func(s *server) { srvCh <- s }
	t.Cleanup(func() { testHookServer = nil })
	h := startTestHubWith(t, allowFake)
	e := &jobEnv{h: h, srv: <-srvCh, adminKey: h.init.AdminAPIKey, base: h.ready.HubURL,
		nodes: map[string]*enrolled{}, sn: map[string]*standIn{}}
	e.adminClients = h.clients(e.adminKey, nil)
	e.anonClients = h.clients("", nil)
	e.aliceID, e.aliceKey = createUser(t, e.adminClients, "alice", mycelv1.Role_ROLE_MEMBER)
	e.bobID, e.bobKey = createUser(t, e.adminClients, "bob", mycelv1.Role_ROLE_MEMBER)
	e.carolID, e.carolKey = createUser(t, e.adminClients, "carol", mycelv1.Role_ROLE_MEMBER)
	e.bobClients = h.clients(e.bobKey, nil)
	hc := h.httpClient(nil)
	jobs := func(key string) mycelv1connect.JobServiceClient {
		return mycelv1connect.NewJobServiceClient(hc, h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
	}
	arts := func(key string) mycelv1connect.ArtifactServiceClient {
		return mycelv1connect.NewArtifactServiceClient(hc, h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
	}
	e.alice, e.bob, e.carol, e.adminJobs = jobs(e.aliceKey), jobs(e.bobKey), jobs(e.carolKey), jobs(e.adminKey)
	e.aliceArt, e.bobArt, e.carolArt = arts(e.aliceKey), arts(e.bobKey), arts(e.carolKey)
	e.aliceCh = mycelv1connect.NewChannelServiceClient(hc, h.ready.HubURL, connect.WithInterceptors(streamBearer(e.aliceKey)))
	e.aliceAgents = h.clients(e.aliceKey, nil).agents

	for _, n := range sandboxNodes {
		e.addNode(t, e.bobClients, n, []string{"sandbox"})
	}
	e.addNode(t, h.clients(e.aliceKey, nil), "n0", nil)
	e.w1, e.sw1 = e.nodes["w1"], e.sn["w1"]
	e.n0, e.sn0 = e.nodes["n0"], e.sn["n0"]
	e.ag = registerAgent(t, e.sn0, "n0", "ag")
	e.aliceHTTP = h.httpClient(nil)
	if e.w1 != nil {
		e.nodeW1HTTP = h.httpClient(&e.w1.cert)
	}
	return e
}

// addNode enrols, connects and reports a node with the fake backend.
func (e *jobEnv) addNode(t *testing.T, owner clients, name string, caps []string) {
	t.Helper()
	en := enrol(t, e.anonClients, createToken(t, owner, name, caps, ""), name)
	sn := e.h.connectNode(t, en)
	beat(t, sn, name, []string{"fake"})
	eventually(t, 5*time.Second, name+" inventory", func() bool {
		return slices.Contains(getNode(t, e.adminClients, name).GetInventory().GetSandboxBackends(), "fake")
	})
	e.nodes[name], e.sn[name] = en, sn
}

func beat(t *testing.T, sn *standIn, node string, backends []string) {
	t.Helper()
	b, err := proto.Marshal(&mycelv1.Heartbeat{NodeName: node, Inventory: &mycelv1.Inventory{Os: "linux", SandboxBackends: backends}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sn.nc.Publish(bus.NodeTelemetry(node), b); err != nil {
		t.Fatal(err)
	}
	if err := sn.nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// waitSandboxCtl waits for a CreateSandbox (create) or KillSandbox for job
// on a stand-in node, skipping other control messages.
func waitSandboxCtl(t *testing.T, sn *standIn, job string, create bool) *mycelv1.CtlRequest {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case req := <-sn.ctl:
			if c := req.GetCreateSandbox(); create && c != nil && (job == "" || c.GetJob().GetId() == job) {
				return req
			}
			if k := req.GetKillSandbox(); !create && k != nil && k.GetJobId() == job {
				return req
			}
		case <-deadline:
			t.Fatalf("no sandbox control (create=%v) for job %s", create, job)
			return nil
		}
	}
}

// waitCreateAny waits for job's CreateSandbox on any of the env's nodes
// and returns the node's name.
func (e *jobEnv) waitCreateAny(t *testing.T, job string) string {
	t.Helper()
	var node string
	eventually(t, 10*time.Second, "CreateSandbox for "+job, func() bool {
		node = e.get(t, e.adminJobs, job).GetNode()
		return node != ""
	})
	waitSandboxCtl(t, e.sn[node], job, true)
	return node
}

func (e *jobEnv) run(t *testing.T, c mycelv1connect.JobServiceClient, spec *mycelv1.JobSpec) (*mycelv1.Job, error) {
	t.Helper()
	r, err := c.RunJob(context.Background(), connect.NewRequest(&mycelv1.RunJobRequest{Spec: spec}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}

func (e *jobEnv) mustRun(t *testing.T, c mycelv1connect.JobServiceClient, spec *mycelv1.JobSpec) *mycelv1.Job {
	t.Helper()
	j, err := e.run(t, c, spec)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	return j
}

func (e *jobEnv) get(t *testing.T, c mycelv1connect.JobServiceClient, id string) *mycelv1.Job {
	t.Helper()
	r, err := c.GetJob(context.Background(), connect.NewRequest(&mycelv1.GetJobRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetJob %s: %v", id, err)
	}
	return r.Msg.GetJob()
}

func (e *jobEnv) waitState(t *testing.T, id string, st mycelv1.JobState) *mycelv1.Job {
	t.Helper()
	var j *mycelv1.Job
	eventually(t, 10*time.Second, "job "+id+" "+st.String(), func() bool {
		j = e.get(t, e.adminJobs, id)
		return j.GetState() == st
	})
	return j
}

func nodeRPC(t *testing.T, sn *standIn, node, method string, req, resp proto.Message) error {
	t.Helper()
	_, err := sn.rpc(t, node, method, req, resp)
	return err
}

func (e *jobEnv) started(t *testing.T, node, job, sbx string) {
	t.Helper()
	if err := nodeRPC(t, e.sn[node], node, bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: job, SandboxId: sbx, Backend: "fake"}, &mycelv1.JobStartedResponse{}); err != nil {
		t.Fatalf("job.started %s: %v", job, err)
	}
}

func (e *jobEnv) done(t *testing.T, node, job string, st mycelv1.JobState, arts ...string) {
	t.Helper()
	if err := nodeRPC(t, e.sn[node], node, bus.RPCJobDone, &mycelv1.JobDoneRequest{JobId: job, State: st, Artifacts: arts}, &mycelv1.JobDoneResponse{}); err != nil {
		t.Fatalf("job.done %s: %v", job, err)
	}
}

func (e *jobEnv) logChunkOn(t *testing.T, node, job string, seq uint64, data string) uint64 {
	t.Helper()
	var ack mycelv1.JobLogAck
	if err := nodeRPC(t, e.sn[node], node, bus.RPCJobLog, &mycelv1.JobLogChunk{JobId: job, Seq: seq, Data: []byte(data)}, &ack); err != nil {
		t.Fatalf("job.log %d: %v", seq, err)
	}
	return ack.GetSeq()
}

func (e *jobEnv) logChunk(t *testing.T, job string, seq uint64, data string) uint64 {
	t.Helper()
	return e.logChunkOn(t, "w1", job, seq, data)
}

// putArtifact uploads body and returns the artifact or the HTTP status.
func putArtifact(t *testing.T, hc *http.Client, base, key, query string, body io.Reader) (*mycelv1.Artifact, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, base+"/api/artifacts?"+query, body)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, resp.StatusCode
	}
	var a mycelv1.Artifact
	if err := protojson.Unmarshal(b, &a); err != nil {
		t.Fatalf("artifact json: %v (%s)", err, b)
	}
	return &a, resp.StatusCode
}

func putBytes(t *testing.T, hc *http.Client, base, key, query string, data []byte) (*mycelv1.Artifact, int) {
	t.Helper()
	return putArtifact(t, hc, base, key, query, bytes.NewReader(data))
}

func getArtifactBytes(t *testing.T, hc *http.Client, base, key, id string, hdr ...string) ([]byte, int, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/artifacts/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 == 2 && resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("artifact served as %q", resp.Header.Get("Content-Type"))
	}
	return b, resp.StatusCode, resp.Header
}

func (e *jobEnv) resultsIn(t *testing.T, channel string) []*mycelv1.Message {
	t.Helper()
	r, err := e.aliceCh.ListMessages(context.Background(), connect.NewRequest(&mycelv1.ListMessagesRequest{Channel: channel, Limit: 500}))
	if err != nil {
		t.Fatal(err)
	}
	var out []*mycelv1.Message
	for _, m := range r.Msg.GetMessages() {
		if m.GetType() == mycelv1.MessageType_MESSAGE_TYPE_RESULT {
			out = append(out, m)
		}
	}
	return out
}

func fake(cmd ...string) *mycelv1.JobSpec { return &mycelv1.JobSpec{Profile: "fake", Command: cmd} }

func TestJobsEndToEnd(t *testing.T) {
	e := newJobEnv(t, true)
	ctx := context.Background()
	base := e.base

	// ---- profiles ----
	lp, err := e.alice.ListProfiles(ctx, connect.NewRequest(&mycelv1.ListProfilesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range lp.Msg.GetProfiles() {
		names = append(names, p.GetName())
	}
	for _, want := range []string{"default", "dev", "fake"} {
		if !slices.Contains(names, want) {
			t.Fatalf("profiles %v lack %s", names, want)
		}
	}
	ln, err := e.alice.ListNodesForProfile(ctx, connect.NewRequest(&mycelv1.ListNodesForProfileRequest{Profile: "fake"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ln.Msg.GetNodes(), []string{"w1"}) {
		t.Fatalf("nodes for fake: %v", ln.Msg.GetNodes())
	}
	ln, err = e.alice.ListNodesForProfile(ctx, connect.NewRequest(&mycelv1.ListNodesForProfileRequest{Profile: "dev"}))
	if err != nil || len(ln.Msg.GetNodes()) != 0 {
		t.Fatalf("nodes for dev: %v %v", ln.Msg.GetNodes(), err)
	}

	// ---- refused submissions ----
	t.Run("refusals", func(t *testing.T) {
		for name, env := range map[string]map[string]string{
			"mycel": {"MYCEL_HOME": "/tmp/x"}, "lower": {"lower": "x"}, "proxy": {"HTTPS_PROXY": "http://evil"},
			"all proxy": {"ALL_PROXY": "x"}, "no proxy": {"NO_PROXY": "*"},
		} {
			spec := fake("true")
			spec.Env = env
			_, err := e.run(t, e.alice, spec)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("env %s: %v", name, err)
			}
		}
		_, err := e.run(t, e.alice, &mycelv1.JobSpec{Profile: "fake"})
		wantCode(t, err, connect.CodeInvalidArgument)
		spec := fake("true")
		spec.Timeout = "1h" // fake wall clock is 10m
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodeInvalidArgument)
		spec = fake("true")
		spec.Outputs = []string{"../escape"}
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodeInvalidArgument)
		for _, u := range []string{"https://localhost/x.git", "https://10.0.0.1/x.git", "https://git.internal/x.git", "https://user:pw@github.com/x"} {
			spec = fake("true")
			spec.Workspace = &mycelv1.WorkspaceSeed{Source: &mycelv1.WorkspaceSeed_Git{Git: &mycelv1.GitSource{Url: u}}}
			_, err = e.run(t, e.alice, spec)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("git %s: %v", u, err)
			}
		}
		spec = fake("true")
		spec.Node = "n0" // sandbox not enabled there
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodeFailedPrecondition)
		spec.Node = "nope"
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodeNotFound)
		spec = fake("true")
		spec.Channel = "#nowhere"
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodeNotFound)
		// The result would be posted for alice, who cannot post in #alerts.
		spec.Channel = "#alerts"
		_, err = e.run(t, e.alice, spec)
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.run(t, e.alice, &mycelv1.JobSpec{Profile: "trainer", Command: []string{"x"}})
		wantCode(t, err, connect.CodeInvalidArgument)
		_, err = e.run(t, mycelv1connect.NewJobServiceClient(e.h.httpClient(nil), base), fake("true"))
		wantCode(t, err, connect.CodeUnauthenticated)
	})

	// ---- a channel and an input artifact ----
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#build", Members: []string{"user:" + e.bobID},
	})); err != nil {
		t.Fatal(err)
	}
	input, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=input.txt", []byte("input data"))
	if code != http.StatusCreated || input.GetOwnerId() != e.aliceID || input.GetCreatedBy() != "user:"+e.aliceID ||
		input.GetSize() != 10 || !strings.HasPrefix(input.GetMediaType(), "text/plain") {
		t.Fatalf("input upload: %d %+v", code, input)
	}
	unrelated, _ := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=other.bin", []byte{0, 1, 2})
	for _, bad := range []string{".hidden", "a/b", "a%5Cb", "a%07b", ""} {
		if _, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name="+bad, []byte("x")); code != http.StatusBadRequest {
			t.Errorf("name %q: %d", bad, code)
		}
	}
	if _, code := putBytes(t, e.aliceHTTP, base, e.carolKey, "name=x&channel=%23build", []byte("x")); code != http.StatusForbidden {
		t.Fatalf("carol upload into #build: %d", code)
	}
	if _, code := putBytes(t, e.aliceHTTP, base, "", "name=x", []byte("x")); code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload: %d", code)
	}
	// Spec references must be visible to the submitter; invisible ones are missing.
	spec := fake("true")
	spec.Inputs = []*mycelv1.JobInput{{Artifact: input.GetId()}}
	_, err = e.run(t, e.carol, spec)
	wantCode(t, err, connect.CodeNotFound)

	// ---- run: queued -> starting -> running ----
	spec = fake("sh", "-c", "echo hi")
	spec.Name, spec.Channel = "build", "#build"
	spec.Inputs = []*mycelv1.JobInput{{Artifact: input.GetId()}}
	spec.Outputs = []string{"out/*"}
	job := e.mustRun(t, e.alice, spec)
	if job.GetState() != mycelv1.JobState_JOB_STATE_QUEUED || job.GetOwnerId() != e.aliceID ||
		job.GetSubmittedBy() != "user:"+e.aliceID || job.GetSpec().GetTimeout() != "10m0s" ||
		job.GetSpec().GetInputs()[0].GetPath() != "inputs/input.txt" {
		t.Fatalf("submitted: %+v", job)
	}
	id := job.GetId()
	create := waitSandboxCtl(t, e.sw1, id, true).GetCreateSandbox()
	wc, _ := time.ParseDuration(create.GetProfile().GetLimits().GetWallClock())
	if create.GetProfile().GetName() != "fake" || wc != 10*time.Minute ||
		create.GetJob().GetState() != mycelv1.JobState_JOB_STATE_STARTING || create.GetJob().GetNode() != "w1" ||
		create.GetInputBaseUrl() != "https://localhost:7777" || !slices.Equal(create.GetJob().GetSpec().GetCommand(), spec.Command) {
		t.Fatalf("CreateSandbox: %+v", create)
	}
	if st := e.get(t, e.alice, id).GetState(); st != mycelv1.JobState_JOB_STATE_STARTING {
		t.Fatalf("after create: %v", st)
	}

	t.Run("node fetches inputs", func(t *testing.T) {
		b, code, _ := getArtifactBytes(t, e.nodeW1HTTP, base, "", input.GetId())
		if code != http.StatusOK || string(b) != "input data" {
			t.Fatalf("node input fetch: %d %q", code, b)
		}
		if _, code, _ := getArtifactBytes(t, e.nodeW1HTTP, base, "", unrelated.GetId()); code != http.StatusNotFound {
			t.Fatalf("node fetch of an unrelated artifact: %d", code)
		}
		if _, code, _ := getArtifactBytes(t, e.h.httpClient(&e.n0.cert), base, "", input.GetId()); code != http.StatusNotFound {
			t.Fatalf("other node fetch: %d", code)
		}
	})

	// job.started from the wrong node is refused.
	err = nodeRPC(t, e.sn0, "n0", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: id, SandboxId: "sbx:1", Backend: "fake"}, &mycelv1.JobStartedResponse{})
	wantPrefix(t, err, "permission_denied")
	err = nodeRPC(t, e.sw1, "w1", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: id, SandboxId: "sbx:1", Backend: "applevf"}, &mycelv1.JobStartedResponse{})
	wantPrefix(t, err, "invalid_argument")
	e.started(t, "w1", id, "sbx:1")
	if j := e.get(t, e.alice, id); j.GetState() != mycelv1.JobState_JOB_STATE_RUNNING || j.GetSandboxId() != "sbx:1" || j.GetBackend() != "fake" {
		t.Fatalf("after started: %+v", j)
	}

	// ---- logs ----
	streamCtx, cancelStream := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStream()
	stream, err := e.alice.StreamLogs(streamCtx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: id, Follow: true}))
	if err != nil {
		t.Fatal(err)
	}
	type streamed struct {
		data string
		eof  bool
		err  error
	}
	streamDone := make(chan streamed, 1)
	gotChunk := make(chan struct{}, 64)
	go func() {
		var b strings.Builder
		var next uint64
		for stream.Receive() {
			m := stream.Msg()
			if m.GetOffset() != next {
				streamDone <- streamed{err: io.ErrUnexpectedEOF}
				return
			}
			b.Write(m.GetData())
			next += uint64(len(m.GetData()))
			if len(m.GetData()) > 0 {
				gotChunk <- struct{}{}
			}
			if m.GetEof() {
				streamDone <- streamed{data: b.String(), eof: true}
				return
			}
		}
		streamDone <- streamed{data: b.String(), err: stream.Err()}
	}()

	if got := e.logChunk(t, id, 1, "hello\n"); got != 1 {
		t.Fatalf("ack 1: %d", got)
	}
	select {
	case <-gotChunk:
	case got := <-streamDone:
		t.Fatalf("follow stream ended early: %v", got.err)
	case <-time.After(10 * time.Second):
		t.Fatal("follow stream did not deliver the first chunk")
	}
	if got := e.logChunk(t, id, 1, "hello\n"); got != 1 { // duplicate
		t.Fatalf("duplicate ack: %d", got)
	}
	if got := e.logChunk(t, id, 3, "late\n"); got != 1 { // gap
		t.Fatalf("gap ack: %d", got)
	}
	if got := e.logChunk(t, id, 2, "world\n"); got != 2 {
		t.Fatalf("ack 2: %d", got)
	}
	if got := e.logChunk(t, id, 3, "late\n"); got != 3 {
		t.Fatalf("ack 3: %d", got)
	}
	err = nodeRPC(t, e.sn0, "n0", bus.RPCJobLog, &mycelv1.JobLogChunk{JobId: id, Seq: 4, Data: []byte("x")}, &mycelv1.JobLogAck{})
	wantPrefix(t, err, "permission_denied")
	if j := e.get(t, e.alice, id); j.GetLogBytes() != uint64(len("hello\nworld\nlate\n")) {
		t.Fatalf("log bytes %d", j.GetLogBytes())
	}
	si, err := e.srv.ch.js.Stream(ctx, bus.StreamJobLogs)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "JOBLOG entries", func() bool {
		info, err := si.Info(ctx)
		return err == nil && info.State.Msgs >= 3
	})

	// ---- agents: get, logs (long poll), artifact list ----
	t.Run("agent", func(t *testing.T) {
		var gr mycelv1.GetJobResponse
		if err := e.ag.call(t, bus.RPCAgentJobGet, &mycelv1.AgentGetJobRequest{Call: e.ag.ac(), Id: id}, &gr, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if gr.GetJob().GetId() != id {
			t.Fatalf("agent get: %+v", gr.GetJob())
		}
		var lr mycelv1.AgentJobLogsResponse
		if err := e.ag.call(t, bus.RPCAgentJobLogs, &mycelv1.AgentJobLogsRequest{Call: e.ag.ac(), Id: id, MaxBytes: 6}, &lr, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if string(lr.GetData()) != "hello\n" || lr.GetNextByte() != 6 || lr.GetState() != mycelv1.JobState_JOB_STATE_RUNNING {
			t.Fatalf("agent logs: %q %d %v", lr.GetData(), lr.GetNextByte(), lr.GetState())
		}
		// A poll at the end of the log returns the next chunk whether it
		// starts waiting before or after the chunk arrives.
		end := uint64(len("hello\nworld\nlate\n"))
		polled := make(chan *mycelv1.AgentJobLogsResponse, 1)
		go func() {
			var r mycelv1.AgentJobLogsResponse
			if err := e.ag.call(t, bus.RPCAgentJobLogs, &mycelv1.AgentJobLogsRequest{Call: e.ag.ac(), Id: id, FromByte: end, WaitMs: 20000}, &r, 30*time.Second); err != nil {
				t.Error(err)
			}
			polled <- &r
		}()
		if got := e.logChunk(t, id, 4, "more\n"); got != 4 {
			t.Fatalf("ack 4: %d", got)
		}
		select {
		case r := <-polled:
			if string(r.GetData()) != "more\n" || r.GetNextByte() != end+5 {
				t.Fatalf("long poll: %q %d", r.GetData(), r.GetNextByte())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("long poll did not return")
		}
		// An agent sees neither its owner's private uploads nor channels it
		// has not joined.
		var al mycelv1.ListArtifactsResponse
		if err := e.ag.call(t, bus.RPCAgentArtifactList, &mycelv1.AgentListArtifactsRequest{Call: e.ag.ac(), List: &mycelv1.ListArtifactsRequest{}}, &al, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if len(al.GetArtifacts()) != 0 {
			t.Fatalf("agent artifacts: %v", al.GetArtifacts())
		}
		bad := fake("true")
		bad.Inputs = []*mycelv1.JobInput{{Artifact: input.GetId()}}
		err := e.ag.call(t, bus.RPCAgentJobRun, &mycelv1.AgentRunJobRequest{Call: e.ag.ac(), Spec: bad}, &mycelv1.RunJobResponse{}, 5*time.Second)
		wantPrefix(t, err, "not_found")
	})

	// ---- outputs ----
	out, code := putBytes(t, e.nodeW1HTTP, base, "", "name=report.txt&job="+id, []byte("report"))
	if code != http.StatusCreated || out.GetOwnerId() != e.aliceID || out.GetCreatedBy() != "node:w1" ||
		out.GetJobId() != id || out.GetChannel() != "#build" {
		t.Fatalf("node output upload: %d %+v", code, out)
	}
	if _, code := putBytes(t, e.nodeW1HTTP, base, "", "name=x&job="+id+"&channel=%23general", []byte("x")); code != http.StatusForbidden {
		t.Fatalf("node upload into another channel: %d", code)
	}
	if _, code := putBytes(t, e.h.httpClient(&e.n0.cert), base, "", "name=x&job="+id, []byte("x")); code != http.StatusForbidden {
		t.Fatalf("upload for a job of another node: %d", code)
	}
	if _, code := putBytes(t, e.aliceHTTP, base, e.bobKey, "name=x&job="+id, []byte("x")); code != http.StatusForbidden {
		t.Fatalf("upload for another owner's job: %d", code)
	}
	private, _ := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=private.txt", []byte("secret"))
	t.Run("artifact visibility", func(t *testing.T) {
		b, code, hdr := getArtifactBytes(t, e.aliceHTTP, base, e.aliceKey, out.GetId())
		if code != http.StatusOK || string(b) != "report" || hdr.Get("ETag") != `"`+out.GetSha256()+`"` ||
			!strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
			t.Fatalf("owner get: %d %q %v", code, b, hdr)
		}
		b, code, _ = getArtifactBytes(t, e.aliceHTTP, base, e.aliceKey, out.GetId(), "Range", "bytes=1-3")
		if code != http.StatusPartialContent || string(b) != "epo" {
			t.Fatalf("range get: %d %q", code, b)
		}
		if _, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.aliceKey, out.GetId(), "If-None-Match", `"`+out.GetSha256()+`"`); code != http.StatusNotModified {
			t.Fatalf("conditional get: %d", code)
		}
		if _, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.carolKey, out.GetId()); code != http.StatusNotFound {
			t.Fatalf("carol get: %d", code)
		}
		if b, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.bobKey, out.GetId()); code != http.StatusOK || string(b) != "report" {
			t.Fatalf("channel member get: %d %q", code, b)
		}
		if _, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.bobKey, private.GetId()); code != http.StatusNotFound {
			t.Fatalf("bob get of a private artifact: %d", code)
		}
		if _, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.adminKey, private.GetId()); code != http.StatusOK {
			t.Fatalf("admin get: %d", code)
		}
		_, err := e.carolArt.GetArtifact(ctx, connect.NewRequest(&mycelv1.GetArtifactRequest{Id: out.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		r, err := e.bobArt.ListArtifacts(ctx, connect.NewRequest(&mycelv1.ListArtifactsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Msg.GetArtifacts()) != 1 || r.Msg.GetArtifacts()[0].GetId() != out.GetId() {
			t.Fatalf("bob list: %v", r.Msg.GetArtifacts())
		}
		r, err = e.aliceArt.ListArtifacts(ctx, connect.NewRequest(&mycelv1.ListArtifactsRequest{JobId: id}))
		if err != nil || len(r.Msg.GetArtifacts()) != 1 {
			t.Fatalf("alice list by job: %v %v", r.Msg.GetArtifacts(), err)
		}
		r, err = e.aliceArt.ListArtifacts(ctx, connect.NewRequest(&mycelv1.ListArtifactsRequest{}))
		if err != nil || len(r.Msg.GetArtifacts()) != 4 {
			t.Fatalf("alice list: %v %v", r.Msg.GetArtifacts(), err)
		}
		_, err = e.bobArt.DeleteArtifact(ctx, connect.NewRequest(&mycelv1.DeleteArtifactRequest{Id: out.GetId()}))
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.carolArt.DeleteArtifact(ctx, connect.NewRequest(&mycelv1.DeleteArtifactRequest{Id: out.GetId()}))
		wantCode(t, err, connect.CodeNotFound)
		// An input of a running job stays.
		_, err = e.aliceArt.DeleteArtifact(ctx, connect.NewRequest(&mycelv1.DeleteArtifactRequest{Id: input.GetId()}))
		wantCode(t, err, connect.CodeFailedPrecondition)
		if _, err := e.aliceArt.DeleteArtifact(ctx, connect.NewRequest(&mycelv1.DeleteArtifactRequest{Id: private.GetId()})); err != nil {
			t.Fatal(err)
		}
		if _, code, _ := getArtifactBytes(t, e.aliceHTTP, base, e.aliceKey, private.GetId()); code != http.StatusNotFound {
			t.Fatalf("deleted artifact: %d", code)
		}
	})

	// ---- done ----
	err = nodeRPC(t, e.sw1, "w1", bus.RPCJobDone, &mycelv1.JobDoneRequest{JobId: id, State: mycelv1.JobState_JOB_STATE_SUCCEEDED, Artifacts: []string{unrelated.GetId()}}, &mycelv1.JobDoneResponse{})
	wantPrefix(t, err, "invalid_argument")
	if err := nodeRPC(t, e.sw1, "w1", bus.RPCJobDone, &mycelv1.JobDoneRequest{
		JobId: id, State: mycelv1.JobState_JOB_STATE_SUCCEEDED, Artifacts: []string{out.GetId()},
		Stats: &mycelv1.JobStats{CpuSecondsMilli: 1200},
	}, &mycelv1.JobDoneResponse{}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-streamDone:
		if got.err != nil || !got.eof || got.data != "hello\nworld\nlate\nmore\n" {
			t.Fatalf("stream: eof=%v err=%v data=%q", got.eof, got.err, got.data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("follow stream did not end after job.done")
	}
	j := e.get(t, e.alice, id)
	if j.GetState() != mycelv1.JobState_JOB_STATE_SUCCEEDED || !slices.Equal(j.GetArtifacts(), []string{out.GetId()}) ||
		j.GetStats().GetCpuSecondsMilli() != 1200 || j.GetStats().GetFinishedAt() == "" {
		t.Fatalf("done: %+v", j)
	}
	// A repeated job.done is accepted and changes nothing; logs and starts
	// after it are answered "job finished".
	e.done(t, "w1", id, mycelv1.JobState_JOB_STATE_FAILED)
	err = nodeRPC(t, e.sw1, "w1", bus.RPCJobLog, &mycelv1.JobLogChunk{JobId: id, Seq: 5, Data: []byte("x")}, &mycelv1.JobLogAck{})
	wantPrefix(t, err, "failed_precondition: job finished")
	err = nodeRPC(t, e.sw1, "w1", bus.RPCJobLog, &mycelv1.JobLogChunk{JobId: id, Seq: 1, Data: []byte("x")}, &mycelv1.JobLogAck{})
	wantPrefix(t, err, "failed_precondition: job finished")
	err = nodeRPC(t, e.sw1, "w1", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: id, SandboxId: "sbx:9", Backend: "fake"}, &mycelv1.JobStartedResponse{})
	wantPrefix(t, err, "failed_precondition: job finished")
	results := e.resultsIn(t, "#build")
	if len(results) != 1 {
		t.Fatalf("results: %v", results)
	}
	result := results[0]
	prov := result.GetMeta().GetProvenance()
	if result.GetFrom() != "hub" || !strings.Contains(result.GetPayload().GetText(), "succeeded, exit code 0") ||
		len(result.GetPayload().GetParts()) != 1 || result.GetPayload().GetParts()[0].GetKind() != "artifact" ||
		result.GetPayload().GetParts()[0].GetRef() != out.GetId() ||
		!prov.GetUntrusted() || prov.GetNode() != "w1" || prov.GetSandbox() != "sbx:1" || prov.GetOrigin() != "hub" {
		t.Fatalf("result message: %+v", result)
	}
	// Non-follow stream of a finished job ends with eof.
	s2, err := e.alice.StreamLogs(ctx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: id, FromByte: 6}))
	if err != nil {
		t.Fatal(err)
	}
	var rest strings.Builder
	eof := false
	for s2.Receive() {
		rest.Write(s2.Msg().GetData())
		eof = eof || s2.Msg().GetEof()
	}
	if s2.Err() != nil || !eof || rest.String() != "world\nlate\nmore\n" {
		t.Fatalf("replay: %v %v %q", s2.Err(), eof, rest.String())
	}

	// ---- access ----
	t.Run("access", func(t *testing.T) {
		_, err := e.bob.GetJob(ctx, connect.NewRequest(&mycelv1.GetJobRequest{Id: id}))
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.carol.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: id}))
		wantCode(t, err, connect.CodePermissionDenied)
		sl, err := e.bob.StreamLogs(ctx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: id}))
		if err == nil {
			for sl.Receive() {
				t.Fatal("bob received logs")
			}
			err = sl.Err()
		}
		wantCode(t, err, connect.CodePermissionDenied)
		lj, err := e.bob.ListJobs(ctx, connect.NewRequest(&mycelv1.ListJobsRequest{IncludeFinished: true}))
		if err != nil || len(lj.Msg.GetJobs()) != 0 {
			t.Fatalf("bob list: %v %v", lj.Msg.GetJobs(), err)
		}
		lj, err = e.alice.ListJobs(ctx, connect.NewRequest(&mycelv1.ListJobsRequest{IncludeFinished: true}))
		if err != nil || len(lj.Msg.GetJobs()) != 1 {
			t.Fatalf("alice list: %v %v", lj.Msg.GetJobs(), err)
		}
		// Unknown ids: members cannot tell them from foreign jobs; admins can.
		_, err = e.alice.GetJob(ctx, connect.NewRequest(&mycelv1.GetJobRequest{Id: "job_01ARZ3NDEKTSV4RRFFQ69G5FAV"}))
		wantCode(t, err, connect.CodePermissionDenied)
		_, err = e.adminJobs.GetJob(ctx, connect.NewRequest(&mycelv1.GetJobRequest{Id: "job_01ARZ3NDEKTSV4RRFFQ69G5FAV"}))
		wantCode(t, err, connect.CodeNotFound)
	})

	// ---- the result is posted only while the submitter may post ----
	t.Run("result re-checked at post time", func(t *testing.T) {
		spec := fake("true")
		spec.Channel = "#build"
		bj := e.mustRun(t, e.bob, spec)
		waitSandboxCtl(t, e.sw1, bj.GetId(), true)
		e.started(t, "w1", bj.GetId(), "sbx:b")
		if _, err := e.aliceCh.RemoveMember(ctx, connect.NewRequest(&mycelv1.RemoveMemberRequest{Channel: "#build", Member: "user:" + e.bobID})); err != nil {
			t.Fatal(err)
		}
		e.done(t, "w1", bj.GetId(), mycelv1.JobState_JOB_STATE_SUCCEEDED)
		for _, m := range e.resultsIn(t, "#build") {
			if strings.Contains(m.GetPayload().GetText(), bj.GetId()) {
				t.Fatalf("result posted for a submitter who left the channel: %s", m.GetPayload().GetText())
			}
		}
	})

	// ---- cancel while running ----
	t.Run("cancel running", func(t *testing.T) {
		job := e.mustRun(t, e.alice, fake("sleep", "100"))
		waitSandboxCtl(t, e.sw1, job.GetId(), true)
		e.started(t, "w1", job.GetId(), "sbx:2")
		cr, err := e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: job.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if cr.Msg.GetJob().GetState() != mycelv1.JobState_JOB_STATE_RUNNING {
			t.Fatalf("cancel answer: %v", cr.Msg.GetJob().GetState())
		}
		waitSandboxCtl(t, e.sw1, job.GetId(), false)
		if err := nodeRPC(t, e.sw1, "w1", bus.RPCJobDone, &mycelv1.JobDoneRequest{JobId: job.GetId(), State: mycelv1.JobState_JOB_STATE_FAILED, ExitCode: 137}, &mycelv1.JobDoneResponse{}); err != nil {
			t.Fatal(err)
		}
		j := e.waitState(t, job.GetId(), mycelv1.JobState_JOB_STATE_CANCELLED)
		if j.GetExitCode() != 137 {
			t.Fatalf("cancelled: %+v", j)
		}
		_, err = e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: job.GetId()}))
		wantCode(t, err, connect.CodeFailedPrecondition)
	})

	t.Run("cancel starting", func(t *testing.T) {
		job := e.mustRun(t, e.alice, fake("true"))
		waitSandboxCtl(t, e.sw1, job.GetId(), true)
		cr, err := e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: job.GetId()}))
		if err != nil || cr.Msg.GetJob().GetState() != mycelv1.JobState_JOB_STATE_STARTING {
			t.Fatalf("cancel starting: %v %v", cr, err)
		}
		waitSandboxCtl(t, e.sw1, job.GetId(), false)
		e.done(t, "w1", job.GetId(), mycelv1.JobState_JOB_STATE_FAILED)
		e.waitState(t, job.GetId(), mycelv1.JobState_JOB_STATE_CANCELLED)
	})

	// ---- the node refuses the sandbox: requeued, then failed ----
	t.Run("node refuses", func(t *testing.T) {
		e.sw1.refuse.Store("backend fake unavailable")
		defer e.sw1.refuse.Store("")
		spec := fake("true")
		spec.Timeout = "2s"
		job := e.mustRun(t, e.alice, spec)
		waitSandboxCtl(t, e.sw1, job.GetId(), true)
		j := e.waitState(t, job.GetId(), mycelv1.JobState_JOB_STATE_FAILED)
		if j.GetError() != "no node available; last attempt: node w1: backend fake unavailable" || j.GetNode() != "" {
			t.Fatalf("refused: %+v", j)
		}
		err = nodeRPC(t, e.sw1, "w1", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: job.GetId(), SandboxId: "sbx:3", Backend: "fake"}, &mycelv1.JobStartedResponse{})
		wantPrefix(t, err, "permission_denied")
	})

	// ---- no eligible node ----
	t.Run("no node", func(t *testing.T) {
		job := e.mustRun(t, e.alice, &mycelv1.JobSpec{Profile: "default", Command: []string{"true"}, Timeout: "1500ms"})
		if st := e.get(t, e.alice, job.GetId()).GetState(); st != mycelv1.JobState_JOB_STATE_QUEUED {
			t.Fatalf("without a node: %v", st)
		}
		j := e.waitState(t, job.GetId(), mycelv1.JobState_JOB_STATE_FAILED)
		if j.GetError() != "no node available" {
			t.Fatalf("no node: %+v", j)
		}
		// A queued job is cancelled at once, and no result is posted for it.
		spec := &mycelv1.JobSpec{Profile: "default", Command: []string{"true"}, Channel: "#build", Name: "queued-cancel"}
		job = e.mustRun(t, e.alice, spec)
		cr, err := e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: job.GetId()}))
		if err != nil || cr.Msg.GetJob().GetState() != mycelv1.JobState_JOB_STATE_CANCELLED {
			t.Fatalf("cancel queued: %v %v", cr, err)
		}
		for _, m := range e.resultsIn(t, "#build") {
			if strings.Contains(m.GetPayload().GetText(), job.GetId()) {
				t.Fatal("result posted for a job cancelled in the queue")
			}
		}
	})

	// ---- agent submissions ----
	t.Run("agent run", func(t *testing.T) {
		var rr mycelv1.RunJobResponse
		if err := e.ag.call(t, bus.RPCAgentJobRun, &mycelv1.AgentRunJobRequest{Call: e.ag.ac(), Spec: fake("echo", "agent")}, &rr, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if rr.GetJob().GetOwnerId() != e.aliceID || rr.GetJob().GetSubmittedBy() != e.ag.id {
			t.Fatalf("agent job: %+v", rr.GetJob())
		}
		aj := rr.GetJob().GetId()
		waitSandboxCtl(t, e.sw1, aj, true)
		e.started(t, "w1", aj, "sbx:ag")
		// The agent sees outputs of jobs it submitted.
		aout, code := putBytes(t, e.nodeW1HTTP, base, "", "name=agent.txt&job="+aj, []byte("a"))
		if code != http.StatusCreated {
			t.Fatalf("agent job output: %d", code)
		}
		var al mycelv1.ListArtifactsResponse
		if err := e.ag.call(t, bus.RPCAgentArtifactList, &mycelv1.AgentListArtifactsRequest{Call: e.ag.ac(), List: &mycelv1.ListArtifactsRequest{}}, &al, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if len(al.GetArtifacts()) != 1 || al.GetArtifacts()[0].GetId() != aout.GetId() {
			t.Fatalf("agent artifacts: %v", al.GetArtifacts())
		}
		var cr mycelv1.CancelJobResponse
		if err := e.ag.call(t, bus.RPCAgentJobCancel, &mycelv1.AgentCancelJobRequest{Call: e.ag.ac(), Id: aj}, &cr, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		e.done(t, "w1", aj, mycelv1.JobState_JOB_STATE_FAILED)
		e.waitState(t, aj, mycelv1.JobState_JOB_STATE_CANCELLED)
		bad := fake("true")
		bad.Env = map[string]string{"MYCEL_LOCAL_ADDR": "x"}
		err := e.ag.call(t, bus.RPCAgentJobRun, &mycelv1.AgentRunJobRequest{Call: e.ag.ac(), Spec: bad}, &rr, 5*time.Second)
		wantPrefix(t, err, "invalid_argument")
		// Agents cannot read a job of another owner.
		bj := e.mustRun(t, e.bob, &mycelv1.JobSpec{Profile: "default", Command: []string{"true"}})
		err = e.ag.call(t, bus.RPCAgentJobGet, &mycelv1.AgentGetJobRequest{Call: e.ag.ac(), Id: bj.GetId()}, &mycelv1.GetJobResponse{}, 5*time.Second)
		wantPrefix(t, err, "permission_denied")
		// A paused agent cannot run jobs.
		if _, err := e.aliceAgents.PauseAgent(ctx, connect.NewRequest(&mycelv1.PauseAgentRequest{Id: e.ag.id})); err != nil {
			t.Fatal(err)
		}
		err = e.ag.call(t, bus.RPCAgentJobRun, &mycelv1.AgentRunJobRequest{Call: e.ag.ac(), Spec: fake("true")}, &rr, 5*time.Second)
		wantPrefix(t, err, "failed_precondition: agent paused")
	})

	// ---- removing a node ends its jobs; a new node of that name cannot touch them ----
	t.Run("node identity", func(t *testing.T) {
		running := e.mustRun(t, e.alice, fake("sleep", "100"))
		waitSandboxCtl(t, e.sw1, running.GetId(), true)
		e.started(t, "w1", running.GetId(), "sbx:r")
		second := e.mustRun(t, e.alice, fake("sleep", "100"))
		waitSandboxCtl(t, e.sw1, second.GetId(), true)
		// w1 is at capacity (2): a job pinned to it waits in the queue.
		pinned := fake("true")
		pinned.Node = "w1"
		pj := e.mustRun(t, e.alice, pinned)
		if st := e.get(t, e.alice, pj.GetId()).GetState(); st != mycelv1.JobState_JOB_STATE_QUEUED {
			t.Fatalf("pinned job at capacity: %v", st)
		}
		if _, err := e.bobClients.nodes.RemoveNode(ctx, connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: "w1"})); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{running.GetId(), second.GetId(), pj.GetId()} {
			if j := e.get(t, e.alice, id); j.GetState() != mycelv1.JobState_JOB_STATE_FAILED || j.GetError() != "node removed" {
				t.Fatalf("job %s after removal: %+v", id, j)
			}
		}
		again := enrol(t, e.anonClients, createToken(t, e.bobClients, "w1", []string{"sandbox"}, ""), "w1")
		sn := e.h.connectNode(t, again)
		e.sn["w1"] = sn
		err := nodeRPC(t, sn, "w1", bus.RPCJobDone, &mycelv1.JobDoneRequest{JobId: running.GetId(), State: mycelv1.JobState_JOB_STATE_SUCCEEDED}, &mycelv1.JobDoneResponse{})
		wantPrefix(t, err, "permission_denied")
		err = nodeRPC(t, sn, "w1", bus.RPCJobLog, &mycelv1.JobLogChunk{JobId: running.GetId(), Seq: 1, Data: []byte("x")}, &mycelv1.JobLogAck{})
		wantPrefix(t, err, "permission_denied")
		if _, code := putBytes(t, e.h.httpClient(&again.cert), base, "", "name=x&job="+running.GetId(), []byte("x")); code != http.StatusForbidden {
			t.Fatalf("new node upload for an old job: %d", code)
		}
		if j := e.get(t, e.alice, running.GetId()); j.GetState() != mycelv1.JobState_JOB_STATE_FAILED {
			t.Fatalf("old job changed: %+v", j)
		}
	})
}

// TestJobTimings covers load balancing, the log cap and the hub's own
// deadlines with short timings and two sandbox nodes.
func TestJobTimings(t *testing.T) {
	cfg := newJobConfig
	cfg.cancelConfirm, cfg.grace, cfg.createTimeout = 500*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond
	cfg.maxJobLog = 10
	e := newJobEnvWith(t, true, cfg, "w1", "w2")
	ctx := context.Background()

	ln, err := e.alice.ListNodesForProfile(ctx, connect.NewRequest(&mycelv1.ListNodesForProfileRequest{Profile: "fake"}))
	if err != nil || !slices.Equal(ln.Msg.GetNodes(), []string{"w1", "w2"}) {
		t.Fatalf("nodes for fake: %v %v", ln.Msg.GetNodes(), err)
	}

	t.Run("load balancing", func(t *testing.T) {
		a, b := e.mustRun(t, e.alice, fake("a")), e.mustRun(t, e.alice, fake("b"))
		na, nb := e.waitCreateAny(t, a.GetId()), e.waitCreateAny(t, b.GetId())
		if na == nb {
			t.Fatalf("both jobs on %s", na)
		}
		for id, node := range map[string]string{a.GetId(): na, b.GetId(): nb} {
			e.started(t, node, id, "sbx:"+id[len(id)-4:])
			e.done(t, node, id, mycelv1.JobState_JOB_STATE_SUCCEEDED)
		}
	})

	t.Run("log cap", func(t *testing.T) {
		j := e.mustRun(t, e.alice, fake("yes"))
		node := e.waitCreateAny(t, j.GetId())
		e.started(t, node, j.GetId(), "sbx:log")
		if e.logChunkOn(t, node, j.GetId(), 1, "12345678") != 1 || e.logChunkOn(t, node, j.GetId(), 2, "abcdefgh") != 2 ||
			e.logChunkOn(t, node, j.GetId(), 3, "more") != 3 {
			t.Fatal("acks")
		}
		if got := e.get(t, e.alice, j.GetId()); got.GetLogBytes() != 10 {
			t.Fatalf("log bytes %d", got.GetLogBytes())
		}
		var lr mycelv1.AgentJobLogsResponse
		ag := registerAgent(t, e.sn0, "n0", "reader")
		if err := ag.call(t, bus.RPCAgentJobLogs, &mycelv1.AgentJobLogsRequest{Call: ag.ac(), Id: j.GetId()}, &lr, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if string(lr.GetData()) != "12345678ab" {
			t.Fatalf("capped log %q", lr.GetData())
		}
		e.done(t, node, j.GetId(), mycelv1.JobState_JOB_STATE_SUCCEEDED)
	})

	t.Run("cancel not confirmed", func(t *testing.T) {
		j := e.mustRun(t, e.alice, fake("sleep", "100"))
		node := e.waitCreateAny(t, j.GetId())
		e.started(t, node, j.GetId(), "sbx:c")
		if _, err := e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: j.GetId()})); err != nil {
			t.Fatal(err)
		}
		waitSandboxCtl(t, e.sn[node], j.GetId(), false)
		got := e.waitState(t, j.GetId(), mycelv1.JobState_JOB_STATE_CANCELLED)
		if got.GetError() != "node did not confirm" {
			t.Fatalf("cancel expiry: %+v", got)
		}
	})

	t.Run("assigned job timeout", func(t *testing.T) {
		spec := fake("sleep", "100")
		spec.Timeout = "1s"
		j := e.mustRun(t, e.alice, spec)
		node := e.waitCreateAny(t, j.GetId())
		e.started(t, node, j.GetId(), "sbx:t")
		waitSandboxCtl(t, e.sn[node], j.GetId(), false)
		got := e.waitState(t, j.GetId(), mycelv1.JobState_JOB_STATE_TIMED_OUT)
		if got.GetError() != "no result from node "+node {
			t.Fatalf("timeout: %+v", got)
		}
	})

	t.Run("sandbox request not answered", func(t *testing.T) {
		e.sn["w1"].silent.Store(true)
		defer e.sn["w1"].silent.Store(false)
		spec := fake("true")
		spec.Node, spec.Timeout = "w1", "3s"
		j := e.mustRun(t, e.alice, spec)
		waitSandboxCtl(t, e.sn["w1"], j.GetId(), true)
		// No answer within the timeout: the request is withdrawn and the job
		// waits in the queue (w1 is skipped for it).
		waitSandboxCtl(t, e.sn["w1"], j.GetId(), false)
		e.waitState(t, j.GetId(), mycelv1.JobState_JOB_STATE_QUEUED)
		got := e.waitState(t, j.GetId(), mycelv1.JobState_JOB_STATE_FAILED)
		if got.GetError() != "no node available; last attempt: node w1 did not answer the sandbox request" {
			t.Fatalf("unanswered: %+v", got)
		}
	})
}

// TestJobLimits covers the artifact size cap, quotas, the concurrent
// upload cap, the active-job cap and the follow-stream cap.
func TestJobLimits(t *testing.T) {
	cfg := newJobConfig
	cfg.maxArtifactBytes, cfg.ownerQuota, cfg.nodeQuota = 1024, 4096, 2048
	cfg.uploadsPerCaller, cfg.maxActivePerOwner = 1, 2
	e := newJobEnvWith(t, false, cfg)
	ctx := context.Background()
	base := e.base

	t.Run("size cap", func(t *testing.T) {
		if _, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=big", make([]byte, 1025)); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("declared length: %d", code)
		}
		streamed := io.NopCloser(bytes.NewReader(make([]byte, 1025))) // no length: chunked
		if _, code := putArtifact(t, e.aliceHTTP, base, e.aliceKey, "name=big", streamed); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("streamed: %d", code)
		}
		if _, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=ok", make([]byte, 1024)); code != http.StatusCreated {
			t.Fatalf("at the cap: %d", code)
		}
	})

	t.Run("concurrent uploads", func(t *testing.T) {
		pr, pw := io.Pipe()
		first := make(chan int, 1)
		go func() {
			_, code := putArtifact(t, e.aliceHTTP, base, e.aliceKey, "name=slow", pr)
			first <- code
		}()
		if _, err := pw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		// The first upload holds alice's only slot while it streams.
		eventually(t, 5*time.Second, "second upload refused", func() bool {
			_, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=y", []byte("y"))
			return code == http.StatusTooManyRequests
		})
		_ = pw.Close()
		if code := <-first; code != http.StatusCreated {
			t.Fatalf("first upload: %d", code)
		}
	})

	t.Run("owner quota", func(t *testing.T) {
		for {
			_, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=fill", make([]byte, 1000))
			if code == http.StatusInsufficientStorage {
				break
			}
			if code != http.StatusCreated {
				t.Fatalf("fill: %d", code)
			}
		}
		r, err := e.aliceArt.ListArtifacts(ctx, connect.NewRequest(&mycelv1.ListArtifactsRequest{Limit: 500}))
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, a := range r.Msg.GetArtifacts() {
			total += a.GetSize()
		}
		if total > 4096 || total < 3000 {
			t.Fatalf("stored %d bytes against a 4096 byte quota", total)
		}
		// Bob's quota is separate.
		if _, code := putBytes(t, e.aliceHTTP, base, e.bobKey, "name=b", make([]byte, 1000)); code != http.StatusCreated {
			t.Fatalf("bob: %d", code)
		}
	})

	t.Run("node quota", func(t *testing.T) {
		hc := e.h.httpClient(&e.n0.cert) // alice's node: her quota is full
		if _, code := putBytes(t, hc, base, "", "name=n", make([]byte, 1000)); code != http.StatusInsufficientStorage {
			t.Fatalf("node upload against a full owner quota: %d", code)
		}
		q := enrol(t, e.anonClients, createToken(t, e.bobClients, "q1", nil, ""), "q1")
		hq := e.h.httpClient(&q.cert)
		if _, code := putBytes(t, hq, base, "", "name=n1", make([]byte, 1000)); code != http.StatusCreated {
			t.Fatalf("node upload: %d", code)
		}
		if _, code := putBytes(t, hq, base, "", "name=n2", make([]byte, 1000)); code != http.StatusCreated {
			t.Fatalf("node upload: %d", code)
		}
		if _, code := putBytes(t, hq, base, "", "name=n3", make([]byte, 1000)); code != http.StatusInsufficientStorage {
			t.Fatalf("node quota: %d", code)
		}
	})

	t.Run("active jobs", func(t *testing.T) {
		q := &mycelv1.JobSpec{Profile: "default", Command: []string{"true"}}
		first := e.mustRun(t, e.alice, q)
		e.mustRun(t, e.alice, q)
		_, err := e.run(t, e.alice, q)
		wantCode(t, err, connect.CodeResourceExhausted)
		e.mustRun(t, e.bob, q) // per owner

		// Follow streams: at most maxLogStreamsPerUser per user.
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		for i := range maxLogStreamsPerUser {
			if _, err := e.alice.StreamLogs(sctx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: first.GetId(), Follow: true})); err != nil {
				t.Fatalf("stream %d: %v", i, err)
			}
		}
		extra, err := e.alice.StreamLogs(sctx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: first.GetId(), Follow: true}))
		if err == nil {
			for extra.Receive() {
			}
			err = extra.Err()
		}
		wantCode(t, err, connect.CodeResourceExhausted)
	})
}

func TestJobsLowDisk(t *testing.T) {
	cfg := newJobConfig
	cfg.minFree = 1 << 62
	e := newJobEnvWith(t, false, cfg)
	if _, code := putBytes(t, e.aliceHTTP, e.base, e.aliceKey, "name=x", []byte("x")); code != http.StatusInsufficientStorage {
		t.Fatalf("upload with a low disk: %d", code)
	}
}

// TestJobsRestart: after a restart, a job that never started is queued
// again and a running job of a node that stays away ends "node offline".
func TestJobsRestart(t *testing.T) {
	cfg := newJobConfig
	cfg.offlineGrace = time.Second
	withJobConfig(t, cfg)
	opts, res := initTestHub(t)
	h, stop := runTestHub(t, opts, res, true)
	admin := h.clients(res.AdminAPIKey, nil)
	w := enrol(t, h.clients("", nil), createToken(t, admin, "w1", []string{"sandbox"}, ""), "w1")
	sn := h.connectNode(t, w)
	beat(t, sn, "w1", []string{"fake"})
	eventually(t, 5*time.Second, "inventory", func() bool {
		return slices.Contains(getNode(t, admin, "w1").GetInventory().GetSandboxBackends(), "fake")
	})
	jobs := func(h *testHub) mycelv1connect.JobServiceClient {
		return mycelv1connect.NewJobServiceClient(h.httpClient(nil), h.ready.HubURL, connect.WithInterceptors(streamBearer(res.AdminAPIKey)))
	}
	jc := jobs(h)
	run := func() string {
		r, err := jc.RunJob(context.Background(), connect.NewRequest(&mycelv1.RunJobRequest{Spec: fake("sleep", "100")}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetJob().GetId()
	}
	running, starting := run(), run()
	waitSandboxCtl(t, sn, running, true)
	waitSandboxCtl(t, sn, starting, true)
	if err := nodeRPC(t, sn, "w1", bus.RPCJobStarted, &mycelv1.JobStartedRequest{JobId: running, SandboxId: "sbx:1", Backend: "fake"}, &mycelv1.JobStartedResponse{}); err != nil {
		t.Fatal(err)
	}
	stop()
	sn.nc.Close()

	h2, _ := runTestHub(t, opts, res, true)
	jc2 := jobs(h2)
	get := func(id string) *mycelv1.Job {
		r, err := jc2.GetJob(context.Background(), connect.NewRequest(&mycelv1.GetJobRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetJob()
	}
	if j := get(starting); j.GetState() != mycelv1.JobState_JOB_STATE_QUEUED || j.GetNode() != "" {
		t.Fatalf("starting job after restart: %+v", j)
	}
	if j := get(running); j.GetState() != mycelv1.JobState_JOB_STATE_RUNNING {
		t.Fatalf("running job after restart: %+v", j)
	}
	eventually(t, 10*time.Second, "node offline", func() bool {
		j := get(running)
		return j.GetState() == mycelv1.JobState_JOB_STATE_FAILED && j.GetError() == "node offline"
	})
	if j := get(starting); j.GetState() != mycelv1.JobState_JOB_STATE_QUEUED {
		t.Fatalf("queued job: %+v", j)
	}
}

func TestJobsWithoutFakeProfile(t *testing.T) {
	h := startTestHub(t)
	admin := h.clients(h.init.AdminAPIKey, nil)
	_, key := createUser(t, admin, "alice", mycelv1.Role_ROLE_MEMBER)
	jc := mycelv1connect.NewJobServiceClient(h.httpClient(nil), h.ready.HubURL, connect.WithInterceptors(streamBearer(key)))
	ctx := context.Background()
	lp, err := jc.ListProfiles(ctx, connect.NewRequest(&mycelv1.ListProfilesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range lp.Msg.GetProfiles() {
		if p.GetName() == "fake" {
			t.Fatal("fake profile offered without AllowFakeProfile")
		}
	}
	if len(lp.Msg.GetProfiles()) < 2 {
		t.Fatalf("profiles: %v", lp.Msg.GetProfiles())
	}
	_, err = jc.RunJob(ctx, connect.NewRequest(&mycelv1.RunJobRequest{Spec: fake("true")}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = jc.ListNodesForProfile(ctx, connect.NewRequest(&mycelv1.ListNodesForProfileRequest{Profile: "fake"}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestParseProfiles(t *testing.T) {
	ps, err := loadRepoProfiles()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mycelv1.Profile{}
	for _, p := range ps {
		byName[p.GetName()] = p
	}
	dev := byName["dev"]
	if dev == nil || dev.GetVersion() != 1 || dev.GetLimits().GetWallClock() != "2h" || dev.GetLimits().GetMemoryBytes() != 8589934592 ||
		!slices.Contains(dev.GetEgress().GetAllow(), "pypi.org") || !slices.Contains(dev.GetBrokers(), "ollama") {
		t.Fatalf("dev: %+v", dev)
	}
	if f := byName["fake"]; f == nil || !isFakeProfile(f) || isFakeProfile(byName["default"]) {
		t.Fatal("fake detection")
	}
	for _, bad := range []string{
		"name: x\nversion: 0\nbackends: [fake]\nlimits: {wall_clock: 1m}\n",
		"name: x\nversion: 1\nbackends: []\nlimits: {wall_clock: 1m}\n",
		"name: x\nversion: 1\nbackends: [fake]\nlimits: {wall_clock: soon}\n",
		"name: Bad\nversion: 1\nbackends: [fake]\nlimits: {wall_clock: 1m}\n",
		"name: x\nversion: 1\nbackends: [fake]\nlimits: {wall_clock: 1m}\nunknown_key: 1\n",
	} {
		if _, err := parseProfileYAML([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPickNode(t *testing.T) {
	prof := &mycelv1.Profile{Backends: []string{"applevf", "gvisor"}}
	cands := []nodeCand{
		{name: "a", sandbox: true, backends: []string{"gvisor"}, capacity: 2},
		{name: "b", sandbox: true, backends: []string{"applevf"}, capacity: 1},
		{name: "c", sandbox: true, backends: []string{"applevf"}, capacity: 2},
		{name: "d", sandbox: false, backends: []string{"applevf"}, capacity: 2},
		{name: "e", sandbox: true, backends: []string{"fake"}, capacity: 2},
	}
	for _, tc := range []struct {
		name    string
		running map[string]int
		pin     string
		skip    map[string]bool
		want    string
	}{
		{"preferred backend, then name", nil, "", nil, "b"},
		{"fewest running first", map[string]int{"c": 1}, "", nil, "b"},
		{"running beats backend rank", map[string]int{"b": 1, "c": 1}, "", nil, "a"},
		{"full node skipped", map[string]int{"b": 1, "c": 2, "a": 2}, "", nil, ""},
		{"skip list", nil, "", map[string]bool{"b": true}, "c"},
		{"pin", nil, "a", nil, "a"},
		{"pin at capacity", map[string]int{"b": 1}, "b", nil, ""},
		{"pin without sandbox", nil, "d", nil, ""},
		{"pin without backend", nil, "e", nil, ""},
		{"pin unknown", nil, "zz", nil, ""},
	} {
		if got := pickNode(cands, tc.running, prof, tc.pin, tc.skip); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if c := nodeCandidate(&nodeRow{Name: "x", CapsEnabled: []string{"sandbox"}}); c.capacity != 2 {
		t.Errorf("unknown CPUs: capacity %d", c.capacity)
	}
	for cpus, want := range map[int32]int{1: 1, 2: 1, 3: 1, 4: 2, 64: 2} {
		inv, _ := proto.Marshal(&mycelv1.Inventory{CpuCount: cpus})
		if c := nodeCandidate(&nodeRow{Name: "x", Inventory: inv}); c.capacity != want {
			t.Errorf("%d CPUs: capacity %d, want %d", cpus, c.capacity, want)
		}
	}
}

func TestJobSpecHelpers(t *testing.T) {
	for _, p := range []string{"a", "dir/file", "out/*.txt", "a/./b"} {
		if !validRelPath(p) {
			t.Errorf("rejected %q", p)
		}
	}
	for _, p := range []string{"", "/abs", "../x", "a/../b", "a\\b", "C:/x", "a\nb", "a\u0085b"} {
		if validRelPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	if validOutputGlob("a[") {
		t.Error("bad glob accepted")
	}
	if validGitURL("https://github.com/x/y.git") != nil {
		t.Error("rejected a public git url")
	}
	for _, u := range []string{"http://x.org/y", "https://user:pw@x.org/y", "file:///etc", "ext::sh -c x", "",
		"https://localhost/x", "https://a.localhost/x", "https://127.0.0.1/x", "https://[::1]/x", "https://printer.local/x",
		"https://git.corp.internal/x", "https://intranet/x"} {
		if validGitURL(u) == nil {
			t.Errorf("accepted %q", u)
		}
	}
	for _, r := range []string{"--upload-pack=x", "a..b", "a b"} {
		if validGitRef(r) {
			t.Errorf("accepted ref %q", r)
		}
	}
	if !validGitRef("v1.2.3") || !validGitRef("feature/x") || !validGitRef("") {
		t.Error("rejected good ref")
	}
	for _, k := range []string{"HTTP_PROXY", "https_proxy", "ALL_PROXY", "NO_PROXY", "FTP_PROXY"} {
		if !isProxyVar(k) {
			t.Errorf("%s not a proxy variable", k)
		}
	}
	if isProxyVar("PROXYCHAIN") {
		t.Error("PROXYCHAIN is not a proxy variable")
	}
	if got := cleanText("a\x00b\u202ec\nd\u0085e\u2028f\u009bg", 100); got != "abc\ndefg" {
		t.Errorf("cleanText: %q", got)
	}
	if got := cleanText("ééé", 3); got != "é" {
		t.Errorf("cleanText cut: %q", got)
	}
	for _, n := range []string{"report.txt", "a b.tar.gz"} {
		if !validArtifactName(n) {
			t.Errorf("rejected name %q", n)
		}
	}
	for _, n := range []string{"", ".env", "..", "a/b", "a\\b", "a\x01b", strings.Repeat("x", 257)} {
		if validArtifactName(n) {
			t.Errorf("accepted name %q", n)
		}
	}
}
