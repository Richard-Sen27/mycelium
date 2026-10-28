package hub

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// TestArtifactsForAgents covers nested output names, daemon uploads on
// behalf of an agent (created_by) and daemon fetches for an agent
// (Mycel-Agent).
func TestArtifactsForAgents(t *testing.T) {
	e := newJobEnv(t, true)
	ctx := context.Background()
	base := e.base
	n0 := e.h.httpClient(&e.n0.cert) // alice's node, home of agent ag
	q := func(v url.Values) string { return v.Encode() }

	t.Run("nested names", func(t *testing.T) {
		a, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=out%2Fr.txt", []byte("r"))
		if code != http.StatusCreated || a.GetName() != "out/r.txt" {
			t.Fatalf("out/r.txt: %d %+v", code, a)
		}
		_, code, hdr := getArtifactBytes(t, e.aliceHTTP, base, e.aliceKey, a.GetId())
		if code != http.StatusOK || hdr.Get("Content-Disposition") != `attachment; filename=r.txt` {
			t.Fatalf("download: %d %q", code, hdr.Get("Content-Disposition"))
		}
		for _, bad := range []string{"../x", "/abs", "a/.hidden", `a\b`, "a//b", "./a"} {
			if _, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, q(url.Values{"name": {bad}}), []byte("x")); code != http.StatusBadRequest {
				t.Errorf("name %q: %d", bad, code)
			}
		}
		// The default input path keeps only the base name.
		spec := fake("cat", "inputs/r.txt")
		spec.Inputs = []*mycelv1.JobInput{{Artifact: a.GetId()}}
		j := e.mustRun(t, e.alice, spec)
		if p := j.GetSpec().GetInputs()[0].GetPath(); p != "inputs/r.txt" {
			t.Fatalf("default input path %q", p)
		}
		if _, err := e.alice.CancelJob(ctx, connect.NewRequest(&mycelv1.CancelJobRequest{Id: j.GetId()})); err != nil {
			t.Fatal(err)
		}
	})

	// A channel the agent is in, and artifacts in it and outside it.
	if _, err := e.aliceCh.CreateChannel(ctx, connect.NewRequest(&mycelv1.CreateChannelRequest{
		Name: "#docs", Members: []string{e.ag.id},
	})); err != nil {
		t.Fatal(err)
	}
	shared, _ := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=shared.txt&channel=%23docs", []byte("shared"))
	private, _ := putBytes(t, e.aliceHTTP, base, e.aliceKey, "name=private.txt", []byte("private"))

	var made *mycelv1.Artifact
	t.Run("upload for an agent", func(t *testing.T) {
		var code int
		made, code = putBytes(t, n0, base, "", q(url.Values{"name": {"notes/a.txt"}, "created_by": {e.ag.id}}), []byte("note"))
		if code != http.StatusCreated || made.GetCreatedBy() != e.ag.id || made.GetOwnerId() != e.aliceID {
			t.Fatalf("agent upload: %d %+v", code, made)
		}
		// Into a channel the agent is in.
		in, code := putBytes(t, n0, base, "", q(url.Values{"name": {"b.txt"}, "created_by": {e.ag.id}, "channel": {"#docs"}}), []byte("b"))
		if code != http.StatusCreated || in.GetChannel() != "#docs" {
			t.Fatalf("agent upload into #docs: %d %+v", code, in)
		}
		// An agent of another node, an unknown agent and a revoked one.
		other := registerAgent(t, e.sw1, "w1", "wa")
		old := registerAgent(t, e.sn0, "n0", "old")
		if _, err := e.aliceAgents.RevokeAgent(ctx, connect.NewRequest(&mycelv1.RevokeAgentRequest{Id: old.id})); err != nil {
			t.Fatal(err)
		}
		for _, who := range []string{other.id, "agent:ghost@n0", old.id, "agent:wa@n0", "user:" + e.aliceID} {
			if _, code := putBytes(t, n0, base, "", q(url.Values{"name": {"x"}, "created_by": {who}}), []byte("x")); code != http.StatusForbidden {
				t.Errorf("created_by %s: %d", who, code)
			}
		}
		// Users cannot claim an agent.
		if _, code := putBytes(t, e.aliceHTTP, base, e.aliceKey, q(url.Values{"name": {"x"}, "created_by": {e.ag.id}}), []byte("x")); code != http.StatusBadRequest {
			t.Errorf("user with created_by: %d", code)
		}
		// Without it the node is the creator.
		plain, code := putBytes(t, n0, base, "", "name=plain.txt", []byte("p"))
		if code != http.StatusCreated || plain.GetCreatedBy() != "node:n0" {
			t.Fatalf("node upload: %d %+v", code, plain)
		}
	})

	t.Run("fetch for an agent", func(t *testing.T) {
		as := func(hc *http.Client, agent, id string) ([]byte, int) {
			b, code, _ := getArtifactBytes(t, hc, base, "", id, "Mycel-Agent", agent)
			return b, code
		}
		if b, code := as(n0, e.ag.id, shared.GetId()); code != http.StatusOK || string(b) != "shared" {
			t.Fatalf("channel-visible: %d %q", code, b)
		}
		if b, code := as(n0, e.ag.id, made.GetId()); code != http.StatusOK || string(b) != "note" {
			t.Fatalf("own upload: %d %q", code, b)
		}
		if _, code := as(n0, e.ag.id, private.GetId()); code != http.StatusNotFound {
			t.Fatalf("invisible: %d", code)
		}
		// The header must name a live agent of the calling node.
		if _, code := as(e.nodeW1HTTP, e.ag.id, shared.GetId()); code != http.StatusForbidden {
			t.Fatalf("agent of another node: %d", code)
		}
		if _, code := as(n0, "agent:ghost@n0", shared.GetId()); code != http.StatusForbidden {
			t.Fatalf("unknown agent: %d", code)
		}
		// Without the header the node needs a job that uses the artifact.
		if _, code, _ := getArtifactBytes(t, n0, base, "", shared.GetId()); code != http.StatusNotFound {
			t.Fatalf("node without a job: %d", code)
		}
	})
}
