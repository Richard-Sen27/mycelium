package daemon

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
	"github.com/richard-sen27/mycelium/internal/identity"
)

// ---- artifacts over HTTPS ----

type fakeArtifact struct {
	a    *mycelv1.Artifact
	data []byte
}

// fakeArtifacts is the hub's artifact store: PUT and GET /api/artifacts
// with node certificate authentication.
type fakeArtifacts struct {
	mu     sync.Mutex
	next   int
	byID   map[string]*fakeArtifact
	agents []string // Mycel-Agent headers seen on GET
	putErr int      // answer PUT with this status when set
	// redirect, when set, answers every request with a 302 there.
	redirect string
	puts     int
	callers  []string // node names that authenticated
}

func newFakeArtifacts() *fakeArtifacts { return &fakeArtifacts{byID: map[string]*fakeArtifact{}} }

// add stores an artifact directly (as if a human uploaded it).
func (f *fakeArtifacts) add(id, name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id] = &fakeArtifact{a: &mycelv1.Artifact{Id: id, Name: name, Size: int64(len(data))}, data: data}
}

func (f *fakeArtifacts) find(pred func(*mycelv1.Artifact) bool) []*fakeArtifact {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeArtifact
	for _, a := range f.byID {
		if pred(a.a) {
			out = append(out, a)
		}
	}
	return out
}

func (f *fakeArtifacts) redirected(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	to := f.redirect
	f.mu.Unlock()
	if to == "" {
		return false
	}
	http.Redirect(w, r, to, http.StatusFound)
	return true
}

func (f *fakeArtifacts) byIDGet(id string) *fakeArtifact {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byID[id]
}

func (f *fakeArtifacts) node(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	name, err := identity.NodeNameFromCert(testTD, r.TLS.PeerCertificates[0])
	if err != nil {
		return "", false
	}
	f.mu.Lock()
	f.callers = append(f.callers, name)
	f.mu.Unlock()
	return name, true
}

func (f *fakeArtifacts) put(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.node(r); !ok {
		http.Error(w, `{"error":"node certificate required"}`, http.StatusUnauthorized)
		return
	}
	if f.redirected(w, r) {
		return
	}
	f.mu.Lock()
	f.puts++
	perr := f.putErr
	f.mu.Unlock()
	if perr != 0 {
		http.Error(w, `{"error":"refused"}`, perr)
		return
	}
	q := r.URL.Query()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.next++
	id := fmt.Sprintf("art_T%04d", f.next)
	a := &mycelv1.Artifact{
		Id: id, Name: q.Get("name"), Size: int64(len(data)), JobId: q.Get("job"),
		Channel: q.Get("channel"), CreatedBy: q.Get("created_by"), MediaType: "application/octet-stream",
	}
	f.byID[id] = &fakeArtifact{a: a, data: data}
	f.mu.Unlock()
	b, _ := protojson.Marshal(a)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

func (f *fakeArtifacts) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.node(r); !ok {
		http.Error(w, `{"error":"node certificate required"}`, http.StatusUnauthorized)
		return
	}
	if f.redirected(w, r) {
		return
	}
	f.mu.Lock()
	if a := r.Header.Get(agentHeader); a != "" {
		f.agents = append(f.agents, a)
	}
	art := f.byID[r.PathValue("id")]
	f.mu.Unlock()
	if art == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"artifact not found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": art.a.GetName()}))
	w.Header().Set("Content-Length", fmt.Sprint(len(art.data)))
	_, _ = w.Write(art.data)
}

// ---- job RPCs ----

type fakeJobLog struct {
	next uint64 // next expected seq
	data []byte
	recv []uint64 // every seq received, in order
}

// fakeJobs answers job.started, job.log and job.done like the hub.
type fakeJobs struct {
	mu      sync.Mutex
	started map[string]*mycelv1.JobStartedRequest
	logs    map[string]*fakeJobLog
	done    map[string][]*mycelv1.JobDoneRequest
	// dropSeq: the first chunk with this seq is "lost" (not stored, acked
	// below it).
	dropSeq     uint64
	dropped     bool
	doneFails   int    // job.done answers "unavailable" this many times first
	startedErr  string // answer to job.started when set
	doneAttempt int
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{
		started: map[string]*mycelv1.JobStartedRequest{},
		logs:    map[string]*fakeJobLog{},
		done:    map[string][]*mycelv1.JobDoneRequest{},
	}
}

func (f *fakeJobs) serve(method string, data []byte) (proto.Message, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case bus.RPCJobStarted:
		req := &mycelv1.JobStartedRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		if f.startedErr != "" {
			return nil, f.startedErr
		}
		f.started[req.GetJobId()] = req
		return &mycelv1.JobStartedResponse{}, ""
	case bus.RPCJobLog:
		req := &mycelv1.JobLogChunk{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		l := f.logs[req.GetJobId()]
		if l == nil {
			l = &fakeJobLog{next: 1}
			f.logs[req.GetJobId()] = l
		}
		l.recv = append(l.recv, req.GetSeq())
		switch {
		case f.dropSeq != 0 && req.GetSeq() == f.dropSeq && !f.dropped:
			f.dropped = true
		case req.GetSeq() == l.next:
			l.data = append(l.data, req.GetData()...)
			l.next++
		}
		return &mycelv1.JobLogAck{Seq: l.next - 1}, ""
	case bus.RPCJobDone:
		req := &mycelv1.JobDoneRequest{}
		if proto.Unmarshal(data, req) != nil {
			return nil, "invalid_argument: malformed"
		}
		f.doneAttempt++
		if f.doneFails > 0 {
			f.doneFails--
			return nil, "unavailable: hub busy"
		}
		f.done[req.GetJobId()] = append(f.done[req.GetJobId()], req)
		return &mycelv1.JobDoneResponse{}, ""
	}
	return nil, "unknown method"
}

func countSeq(recv []uint64, seq uint64) int {
	n := 0
	for _, s := range recv {
		if s == seq {
			n++
		}
	}
	return n
}
