package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func TestParseGrantResource(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string // protojson of the GrantSpec
		wantErr string
	}{
		{name: "ollama", in: `{"kind":"ollama","models":["llama3.1:8b","qwen"],"allow_pull":true,"requests_per_minute":30,"max_tokens":100000}`,
			want: `{"ollama":{"models":["llama3.1:8b","qwen"],"allow_pull":true,"requests_per_minute":30,"max_tokens":"100000"}}`},
		{name: "ollama without models = every present model", in: `{"kind":"ollama"}`, want: `{"ollama":{}}`},
		{name: "gpu", in: `{"kind":"gpu","device":1,"exclusive":true}`, want: `{"gpu":{"device":1,"exclusive":true}}`},
		{name: "gpu defaults to device 0, time-sliced", in: `{"kind":"gpu"}`, want: `{"gpu":{}}`},
		{name: "gpu mig slice", in: `{"kind":"gpu","device":0,"mig_slice":"1g.10gb"}`, want: `{"gpu":{"mig_slice":"1g.10gb"}}`},
		{name: "docker-host", in: `{"kind":"docker-host","images":["python:3.12","ghcr.io/x/"],"gpu":true,"cpus":4,"memory_bytes":1073741824}`,
			want: `{"docker_host":{"images":["python:3.12","ghcr.io/x/"],"gpu":true,"cpus":4,"memory_bytes":"1073741824"}}`},
		{name: "mount", in: `{"kind":"mount","host_path":"/data/x","guest_path":"/mnt/x","read_only":true}`,
			want: `{"mount":{"host_path":"/data/x","guest_path":"/mnt/x","read_only":true}}`},
		{name: "mount defaults to read-only", in: `{"kind":"mount","host_path":"/data/x","guest_path":"/mnt/x"}`,
			want: `{"mount":{"host_path":"/data/x","guest_path":"/mnt/x","read_only":true}}`},
		{name: "mount read-write only when explicit", in: `{"kind":"mount","host_path":"/data/x","guest_path":"/mnt/x","read_only":false}`,
			want: `{"mount":{"host_path":"/data/x","guest_path":"/mnt/x"}}`},
		{name: "mount read_only null = read-only", in: `{"kind":"mount","host_path":"/d","guest_path":"/m","read_only":null}`,
			want: `{"mount":{"host_path":"/d","guest_path":"/m","read_only":true}}`},
		{name: "egress wildcard", in: `{"kind":"egress","allow":["*.github.com","[::1]:8080","10.0.0.1"]}`, want: `{"egress":{"allow":["*.github.com","[::1]:8080","10.0.0.1"]}}`},
		{name: "list of 64 entries", in: `{"kind":"ollama","models":[` + strings.TrimSuffix(strings.Repeat(`"m",`, 64), ",") + `]}`,
			want: `{"ollama":{"models":[` + strings.TrimSuffix(strings.Repeat(`"m",`, 64), ",") + `]}}`},

		{name: "key case differs", in: `{"kind":"gpu","Device":1}`, wantErr: `field "Device" does not belong to a gpu grant`},
		{name: "kind key case differs", in: `{"Kind":"gpu"}`, wantErr: "resource.kind is required"},
		{name: "kind null", in: `{"kind":null}`, wantErr: "resource.kind is required"},
		{name: "list of 65 entries", in: `{"kind":"egress","allow":[` + strings.TrimSuffix(strings.Repeat(`"a.com",`, 65), ",") + `]}`, wantErr: "65 entries, more than the 64"},
		{name: "entry too long", in: `{"kind":"docker-host","images":["` + strings.Repeat("x", 257) + `"]}`, wantErr: "more than the 256 allowed"},
		{name: "NUL in model", in: `{"kind":"ollama","models":["a\u0000b"]}`, wantErr: "NUL"},
		{name: "NUL in host_path", in: `{"kind":"mount","host_path":"/a\u0000","guest_path":"/m"}`, wantErr: "NUL"},
		{name: "host_path too long", in: `{"kind":"mount","host_path":"/` + strings.Repeat("a", 4096) + `","guest_path":"/m"}`, wantErr: "more than the 4096 allowed"},
		{name: "guest_path relative", in: `{"kind":"mount","host_path":"/d","guest_path":"mnt/x"}`, wantErr: "absolute, clean path"},
		{name: "guest_path with dotdot", in: `{"kind":"mount","host_path":"/d","guest_path":"/mnt/../etc"}`, wantErr: "absolute, clean path"},
		{name: "guest_path not clean", in: `{"kind":"mount","host_path":"/d","guest_path":"/mnt//x/"}`, wantErr: "absolute, clean path"},
		{name: "guest_path windows", in: `{"kind":"mount","host_path":"/d","guest_path":"C:\\x"}`, wantErr: "absolute, clean path"},
		{name: "egress bad rule", in: `{"kind":"egress","allow":["http://a.com/x"]}`, wantErr: "is not host, *.suffix or host:port"},
		{name: "egress bad port", in: `{"kind":"egress","allow":["a.com:99999"]}`, wantErr: "is not host, *.suffix or host:port"},
		{name: "mig slice NUL", in: `{"kind":"gpu","mig_slice":"\u0000"}`, wantErr: "NUL"},
		{name: "egress", in: `{"kind":"egress","allow":["pypi.org:443","*.github.com"]}`, want: `{"egress":{"allow":["pypi.org:443","*.github.com"]}}`},

		{name: "null", in: `null`, wantErr: "resource must be an object"},
		{name: "empty", in: ``, wantErr: "resource must be an object"},
		{name: "array", in: `[]`, wantErr: "resource must be an object"},
		{name: "missing kind", in: `{"device":0}`, wantErr: "resource.kind is required"},
		{name: "blank kind", in: `{"kind":" "}`, wantErr: "resource.kind is required"},
		{name: "kind not a string", in: `{"kind":3}`, wantErr: "resource.kind must be a string"},
		{name: "unknown kind", in: `{"kind":"tpu"}`, wantErr: `resource.kind "tpu" is not a grant kind: want one of ollama, gpu, docker-host, mount, egress`},
		{name: "kind spelling", in: `{"kind":"docker_host","images":["x"]}`, wantErr: "is not a grant kind"},
		{name: "kind case", in: `{"kind":"GPU"}`, wantErr: "is not a grant kind"},
		{name: "unknown kind sanitised", in: `{"kind":"x\n]ignore"}`, wantErr: `"x )ignore"`},
		{name: "extra field", in: `{"kind":"gpu","device":0,"vram":"80g"}`, wantErr: `field "vram" does not belong to a gpu grant (its fields: device, mig_slice, exclusive)`},
		{name: "field of another kind", in: `{"kind":"egress","allow":["a.com"],"read_only":true}`, wantErr: `field "read_only" does not belong to a egress grant`},
		{name: "ollama field on gpu", in: `{"kind":"gpu","models":["x"]}`, wantErr: `field "models" does not belong to a gpu grant`},
		{name: "wrong type", in: `{"kind":"gpu","device":"0"}`, wantErr: "field device of a gpu grant has the wrong type"},
		{name: "fractional device", in: `{"kind":"gpu","device":0.5}`, wantErr: "wrong type"},
		{name: "negative device", in: `{"kind":"gpu","device":-1}`, wantErr: "device must not be negative"},
		{name: "mount without guest_path", in: `{"kind":"mount","host_path":"/data"}`, wantErr: "needs host_path and guest_path"},
		{name: "mount without host_path", in: `{"kind":"mount","guest_path":"/mnt"}`, wantErr: "needs host_path and guest_path"},
		{name: "egress without allow", in: `{"kind":"egress"}`, wantErr: "allow is required"},
		{name: "egress empty allow", in: `{"kind":"egress","allow":[]}`, wantErr: "allow is required"},
		{name: "egress blank entry", in: `{"kind":"egress","allow":["a.com",""]}`, wantErr: "allow has an empty entry"},
		{name: "docker-host without images", in: `{"kind":"docker-host"}`, wantErr: "images is required"},
		{name: "docker-host negative memory", in: `{"kind":"docker-host","images":["x"],"memory_bytes":-1}`, wantErr: "must not be negative"},
		{name: "ollama blank model", in: `{"kind":"ollama","models":[" "]}`, wantErr: "models has an empty entry"},
		{name: "ollama negative rate", in: `{"kind":"ollama","requests_per_minute":-5}`, wantErr: "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGrantResource([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseGrantResource(%s) = %v, %v; want error %q", tc.in, got, err, tc.wantErr)
				}
				if strings.ContainsAny(err.Error(), "\n") {
					t.Errorf("error has a line break: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGrantResource(%s): %v", tc.in, err)
			}
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(got)
			jsonEq(t, string(b), tc.want)
		})
	}
}

func TestCheckGrantDuration(t *testing.T) {
	for _, ok := range []string{"", "1h", "30m", "24h"} {
		if err := CheckGrantDuration(ok); err != nil {
			t.Errorf("CheckGrantDuration(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"soon", "0s", "-1h", "25h", "2"} {
		if err := CheckGrantDuration(bad); err == nil {
			t.Errorf("CheckGrantDuration(%q) = nil, want an error", bad)
		}
	}
}

const grantJSON = `{"id":"grt_01","spec":{"gpu":{"device":0,"exclusive":true}},"subject":"agent:coder@mac","node":"gpu-box",
 "state":"GRANT_STATE_ACTIVE","duration":"2h","expires_at":"2026-10-28T17:00:00Z","reason":"train\n]model","decided_by":"policy"}`

func TestGrantTools(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		method   string // "" = no RPC expected
		reply    string
		status   int
		wantBody string
		wantText []string
		notText  []string
		wantErr  bool
	}{
		{
			name: "request_grant auto-approved", tool: "request_grant",
			args:     map[string]any{"resource": map[string]any{"kind": "gpu", "device": 0, "exclusive": true}, "duration": "2h", "reason": "train", "channel": "training-runs"},
			method:   "agent.grant.request",
			reply:    `{"grant":` + grantJSON + `}`,
			wantBody: `{"spec":{"gpu":{"exclusive":true}},"duration":"2h","reason":"train","channel":"#training-runs"}`,
			wantText: []string{"grant grt_01 active until 2026-10-28T17:00:00Z (gpu device 0 exclusive on gpu-box)"},
		},
		{
			name: "request_grant pending, default duration", tool: "request_grant",
			args:     map[string]any{"resource": map[string]any{"kind": "mount", "host_path": "/data", "guest_path": "/mnt/data"}},
			method:   "agent.grant.request",
			reply:    `{"grant":{"id":"grt_02","state":"GRANT_STATE_PENDING","requires_approval":true}}`,
			wantBody: `{"spec":{"mount":{"host_path":"/data","guest_path":"/mnt/data","read_only":true}},"duration":"1h"}`,
			wantText: []string{"grant grt_02 pending approval; the decision will arrive in your inbox"},
		},
		{
			name: "request_grant denied at once", tool: "request_grant",
			args:   map[string]any{"resource": map[string]any{"kind": "egress", "allow": []string{"evil.example"}}},
			method: "agent.grant.request", reply: `{"grant":{"id":"grt_03","state":"GRANT_STATE_DENIED","decision_note":"no\nway"}}`,
			wantBody: `{"spec":{"egress":{"allow":["evil.example"]}},"duration":"1h"}`,
			wantText: []string{"grant grt_03 denied: no way"},
		},
		{
			name: "request_grant hub refusal", tool: "request_grant",
			args:   map[string]any{"resource": map[string]any{"kind": "ollama"}},
			method: "agent.grant.request", status: http.StatusForbidden, reply: `{"error":"permission_denied: the network is frozen"}`,
			wantBody: `{"spec":{"ollama":{}},"duration":"1h"}`,
			wantText: []string{"the network is frozen"}, wantErr: true,
		},
		{name: "request_grant bad kind", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "tpu"}}, wantErr: true, wantText: []string{"not a grant kind"}},
		{name: "request_grant extra field", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu", "count": 2}}, wantErr: true, wantText: []string{`field "count" does not belong to a gpu grant`}},
		{name: "request_grant missing field", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "mount", "host_path": "/x"}}, wantErr: true, wantText: []string{"needs host_path and guest_path"}},
		{name: "request_grant without resource", tool: "request_grant", args: map[string]any{"duration": "1h"}, wantErr: true, wantText: []string{"resource"}},
		{name: "request_grant direct channel", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "channel": "@agent:x@y"}, wantErr: true, wantText: []string{"must be a #channel name"}},
		{name: "request_grant dm channel", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "channel": "dm:a:b"}, wantErr: true, wantText: []string{"must be a #channel name"}},
		{name: "request_grant long channel", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "channel": "#" + strings.Repeat("c", 128)}, wantErr: true, wantText: []string{"more than the 128 allowed"}},
		{name: "request_grant long reason", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "reason": strings.Repeat("r", 1025)}, wantErr: true, wantText: []string{"more than the 1024 allowed"}},
		{name: "request_grant NUL reason", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "reason": "a\x00b"}, wantErr: true, wantText: []string{"NUL"}},
		{name: "request_grant long duration", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "duration": strings.Repeat("1", 33) + "s"}, wantErr: true, wantText: []string{"longer than 32 bytes"}},
		{
			name: "request_grant reply without grant", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}},
			method: "agent.grant.request", reply: `{}`, wantBody: `{"spec":{"gpu":{}},"duration":"1h"}`, wantErr: true, wantText: []string{"hub returned no grant"},
		},
		{name: "request_grant bad duration", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "duration": "forever"}, wantErr: true, wantText: []string{"invalid duration"}},
		{name: "request_grant too long", tool: "request_grant", args: map[string]any{"resource": map[string]any{"kind": "gpu"}, "duration": "48h"}, wantErr: true, wantText: []string{"24h"}},
		{
			name: "list_grants", tool: "list_grants", method: "agent.grant.list",
			reply:    `{"grants":[` + grantJSON + `,{"id":"grt_02","spec":{"mount":{"host_path":"/data","guest_path":"/mnt/data","read_only":true}},"state":"GRANT_STATE_PENDING","node":"mac","subject":"agent:coder@mac","duration":"1h"}]}`,
			wantBody: `{}`,
			wantText: []string{
				"grt_01 active gpu device 0 exclusive on gpu-box subject agent:coder@mac expires 2026-10-28T17:00:00Z reason \"train )model\" decided by policy\n",
				"grt_02 pending mount /data -> /mnt/data read-only on mac subject agent:coder@mac for 1h",
			},
			notText: []string{"\n]"},
		},
		{name: "list_grants empty", tool: "list_grants", method: "agent.grant.list", reply: `{}`, wantBody: `{}`, wantText: []string{"no grants"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeLocal(t)
			if tc.method != "" {
				st := tc.status
				if st == 0 {
					st = http.StatusOK
				}
				f.set(tc.method, st, tc.reply)
			}
			s := startServe(t, srv.URL)
			before := len(f.requests())
			got, isErr := callTool(t, s, tc.tool, tc.args)
			if isErr != tc.wantErr {
				t.Fatalf("error = %v, want %v: %s", isErr, tc.wantErr, got)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(got, w) {
					t.Errorf("result\n%q\nmissing\n%q", got, w)
				}
			}
			for _, w := range tc.notText {
				if strings.Contains(got, w) {
					t.Errorf("result contains %q: %q", w, got)
				}
			}
			reqs := f.requests()[before:]
			if tc.method == "" {
				if len(reqs) != 0 {
					t.Errorf("unexpected requests %+v", reqs)
				}
				return
			}
			if len(reqs) != 1 || reqs[0].Path != "/api/local/agents/s1/rpc/"+tc.method {
				t.Fatalf("requests = %+v, want one to rpc/%s", reqs, tc.method)
			}
			jsonEq(t, reqs[0].Body, tc.wantBody)
		})
	}
}

func TestGrantSpecText(t *testing.T) {
	tests := []struct {
		spec *mycelv1.GrantSpec
		want string
	}{
		{nil, "-"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{Models: []string{"a", "b"}, AllowPull: true, RequestsPerMinute: 10, MaxTokens: 5}}}, "ollama models a,b pull 10/min max 5 tokens"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Ollama{Ollama: &mycelv1.OllamaGrant{}}}, "ollama every present model"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Gpu{Gpu: &mycelv1.GpuGrant{Device: 1, MigSlice: "1g"}}}, "gpu device 1 mig 1g shared"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_DockerHost{DockerHost: &mycelv1.DockerHostGrant{Images: []string{"x"}, Gpu: true, Cpus: 2}}}, "docker-host images x gpu 2 cpus"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Mount{Mount: &mycelv1.MountGrant{HostPath: "/a\nb", GuestPath: "/c"}}}, "mount /a b -> /c read-write"},
		{&mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Egress{Egress: &mycelv1.EgressGrant{Allow: []string{"a", "b"}}}}, "egress allow a,b"},
	}
	for _, tc := range tests {
		if got := GrantSpecText(tc.spec); got != tc.want {
			t.Errorf("GrantSpecText(%v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
	if GrantStateName(mycelv1.GrantState_GRANT_STATE_REVOKED) != "revoked" || GrantStateName(0) != "-" {
		t.Error("GrantStateName")
	}
}

func TestGrantToolDescriptions(t *testing.T) {
	_, srv := newFakeLocal(t)
	s := startServe(t, srv.URL)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name != "request_grant" {
			continue
		}
		schema, _ := json.Marshal(tl.InputSchema)
		for _, w := range []string{`"required":["resource"]`, "docker-host", "host_path"} {
			if !strings.Contains(string(schema), w) {
				t.Errorf("request_grant schema %s missing %s", schema, w)
			}
		}
		if !strings.Contains(tl.Description, "inbox") || !strings.Contains(tl.Description, "another agent") {
			t.Errorf("request_grant description: %s", tl.Description)
		}
		return
	}
	t.Fatal("request_grant not listed")
}

func TestLocalControlsClient(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			writeJSON(w, http.StatusUnauthorized, `{"error":"bad"}`)
			return
		}
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b[:n]))
		switch r.Method + " " + r.URL.Path {
		case "GET /api/local/sharing":
			writeJSON(w, 200, `{"hub":{"when":"always","max_jobs":4},"local":{},"effective":{"when":"always","max_jobs":4}}`)
		case "PUT /api/local/sharing":
			writeJSON(w, 200, `{"when":"idle","max_jobs":2}`)
		case "GET /api/local/grants":
			writeJSON(w, 200, `{"grants":[{"id":"grt_01","state":"GRANT_STATE_ACTIVE"}]}`)
		case "DELETE /api/local/grants/grt_01":
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/local/freeze":
			writeJSON(w, 200, `{"frozen":true,"scope":"all","reason":"incident"}`)
		default:
			writeJSON(w, 404, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sh, err := c.Sharing(ctx)
	if err != nil || sh.GetHub().GetMaxJobs() != 4 || sh.GetEffective().GetWhen() != "always" {
		t.Fatalf("Sharing = %v, %v", sh, err)
	}
	eff, err := c.SetSharing(ctx, &mycelv1.SharingSettings{When: "idle", MaxJobs: 2})
	if err != nil || eff.GetWhen() != "idle" {
		t.Fatalf("SetSharing = %v, %v", eff, err)
	}
	gs, err := c.LocalGrants(ctx)
	if err != nil || len(gs) != 1 || gs[0].GetId() != "grt_01" {
		t.Fatalf("LocalGrants = %v, %v", gs, err)
	}
	if err := c.RevokeLocalGrant(ctx, "grt_01"); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeLocalGrant(ctx, "grt_02"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("revoke unknown = %v", err)
	}
	fz, err := c.Freeze(ctx)
	if err != nil || !fz.Frozen || fz.Scope != "all" || fz.Reason != "incident" {
		t.Fatalf("Freeze = %+v, %v", fz, err)
	}
	if !strings.Contains(strings.ReplaceAll(strings.Join(got, "\n"), " ", ""), `PUT/api/local/sharing{"when":"idle","max_jobs":2}`) {
		t.Errorf("requests = %q", got)
	}
}
