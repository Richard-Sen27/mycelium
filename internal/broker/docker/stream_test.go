package docker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/broker"
)

// openStream starts a follow=1 logs request and waits for its first line.
func (c *client) openStream(id string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, c.base+"/v1.43/containers/"+id+"/logs?follow=1&stdout=1", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, len("log line\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func expectCut(t *testing.T, resp *http.Response) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not cut")
	}
}

func (a *auditLog) find(f func(broker.Record) bool) (broker.Record, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var hit broker.Record
	n := 0
	for _, r := range a.recs {
		if f(r) {
			hit = r
			n++
		}
	}
	return hit, n
}

func TestStreamsCutAndAudited(t *testing.T) {
	for _, how := range []string{"freeze", "detach"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t, "unix")
			c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
			id := c.create(`{"Image":"alpine"}`)
			resp, err := c.openStream(id)
			if err != nil {
				t.Fatal(err)
			}
			if how == "freeze" {
				e.b.Freeze("network frozen")
				defer e.b.Unfreeze()
			} else if err := e.b.Detach(context.Background(), "g1", "revoked"); err != nil {
				t.Fatal(err)
			}
			expectCut(t, resp)
			isLogs := func(r broker.Record) bool { return r.Op == "GET containers.logs" }
			deadline := time.Now().Add(5 * time.Second)
			for {
				rec, n := e.audit.find(isLogs)
				if n == 1 {
					if !rec.Allowed || rec.Reason != "aborted" {
						t.Errorf("audit %+v", rec)
					}
					break
				}
				if n > 1 || time.Now().After(deadline) {
					t.Fatalf("%d logs audit records", n)
				}
				time.Sleep(time.Millisecond)
			}
			if how == "freeze" {
				if _, n := e.audit.find(func(r broker.Record) bool {
					return r.Op == "POST containers.pause" && r.Target == id && r.Allowed
				}); n != 1 {
					t.Error("pause not audited")
				}
			}
		})
	}
}

func TestStartBodyAndExecAudit(t *testing.T) {
	e := newEnv(t, "unix")
	c := e.attach("g1", &mycelv1.DockerHostGrant{Images: []string{"alpine"}})
	id := c.create(`{"Image":"alpine"}`)
	if code, _ := c.do(http.MethodPost, "/containers/"+id+"/start", `{"Binds":["/:/host"],"Privileged":true}`); code != 204 {
		t.Fatalf("start: %d", code)
	}
	e.eng.mu.Lock()
	body := e.eng.lastBody["/containers/"+id+"/start"]
	e.eng.mu.Unlock()
	if body != "" {
		t.Errorf("start body forwarded: %q", body)
	}
	if code, _ := c.do(http.MethodPost, "/containers/"+id+"/exec", `{"cmd":["sh","-c","id"],"privileged":true}`); code != 400 {
		t.Errorf("lowercase privileged exec: %d", code)
	}
	if code, _ := c.do(http.MethodPost, "/containers/"+id+"/exec", `{"Cmd":"id","Junk":1}`); code != 201 {
		t.Fatalf("exec: %d", code)
	}
	if rec := e.audit.last(); rec.Op != "POST containers.exec" || !strings.Contains(rec.Reason, `cmd ["id"]`) {
		t.Errorf("audit %+v", rec)
	}
	e.eng.mu.Lock()
	body = e.eng.lastBody["/containers/"+id+"/exec"]
	e.eng.mu.Unlock()
	if strings.Contains(body, "Junk") || !strings.Contains(body, `"Privileged":false`) {
		t.Errorf("exec body %s", body)
	}
}
