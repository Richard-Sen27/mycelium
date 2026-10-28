package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
)

// fakeGrants implements GrantService and ControlService for CLI tests.
type fakeGrants struct {
	mycelv1connect.UnimplementedGrantServiceHandler
	mycelv1connect.UnimplementedControlServiceHandler
	hub *fakeHub

	mu       sync.Mutex
	sharing  *mycelv1.SharingSettings // hub-side settings of node "gpu-box"
	setCalls int
}

func newFakeGrants(h *fakeHub) *fakeGrants {
	return &fakeGrants{hub: h, sharing: &mycelv1.SharingSettings{
		Offer: &mycelv1.Capabilities{Names: []string{"docker", "gpu"}}, When: "schedule",
		Schedule:   []*mycelv1.ScheduleWindow{{Days: []string{"mon", "tue", "wed", "thu", "fri"}, From: "22:00", To: "07:00"}},
		LocalFirst: true, MaxJobs: 3, IdleAfterMinutes: 10,
	}}
}

func testGrant(id string, st mycelv1.GrantState) *mycelv1.Grant {
	g := &mycelv1.Grant{
		Id: id, Spec: &mycelv1.GrantSpec{Kind: &mycelv1.GrantSpec_Gpu{Gpu: &mycelv1.GpuGrant{Device: 0, Exclusive: true}}},
		Subject: "agent:trainer@gpu-box", Node: "gpu-box", OwnerId: "usr_1", RequestedBy: "agent:trainer@gpu-box",
		Reason: "train\x1b[2J model", Duration: "2h", State: st, CreatedAt: "2026-10-28T14:50:00Z", RequiresApproval: true,
	}
	if st == mycelv1.GrantState_GRANT_STATE_ACTIVE {
		g.ActivatedAt, g.ExpiresAt, g.DecidedBy = "2026-10-28T15:00:00Z", "2026-10-28T17:00:00Z", "usr_1"
	}
	return g
}

func (f *fakeGrants) RequestGrant(_ context.Context, r *connect.Request[mycelv1.RequestGrantRequest]) (*connect.Response[mycelv1.RequestGrantResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	st := mycelv1.GrantState_GRANT_STATE_PENDING
	if r.Msg.GetSpec().GetGpu() != nil {
		st = mycelv1.GrantState_GRANT_STATE_ACTIVE
	}
	g := testGrant("grt_new", st)
	g.Spec = r.Msg.GetSpec()
	return connect.NewResponse(&mycelv1.RequestGrantResponse{Grant: g}), nil
}

func (f *fakeGrants) ListGrants(_ context.Context, r *connect.Request[mycelv1.ListGrantsRequest]) (*connect.Response[mycelv1.ListGrantsResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListGrantsResponse{Grants: []*mycelv1.Grant{
		testGrant("grt_01", mycelv1.GrantState_GRANT_STATE_ACTIVE), testGrant("grt_02", mycelv1.GrantState_GRANT_STATE_PENDING),
	}}), nil
}

func (f *fakeGrants) GetGrant(_ context.Context, r *connect.Request[mycelv1.GetGrantRequest]) (*connect.Response[mycelv1.GetGrantResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	if r.Msg.GetId() != "grt_01" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("grant "+r.Msg.GetId()))
	}
	return connect.NewResponse(&mycelv1.GetGrantResponse{Grant: testGrant("grt_01", mycelv1.GrantState_GRANT_STATE_ACTIVE)}), nil
}

func (f *fakeGrants) ApproveGrant(_ context.Context, r *connect.Request[mycelv1.ApproveGrantRequest]) (*connect.Response[mycelv1.ApproveGrantResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ApproveGrantResponse{Grant: testGrant(r.Msg.GetId(), mycelv1.GrantState_GRANT_STATE_ACTIVE)}), nil
}

func (f *fakeGrants) DenyGrant(_ context.Context, r *connect.Request[mycelv1.DenyGrantRequest]) (*connect.Response[mycelv1.DenyGrantResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.DenyGrantResponse{Grant: testGrant(r.Msg.GetId(), mycelv1.GrantState_GRANT_STATE_DENIED)}), nil
}

func (f *fakeGrants) RevokeGrant(_ context.Context, r *connect.Request[mycelv1.RevokeGrantRequest]) (*connect.Response[mycelv1.RevokeGrantResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RevokeGrantResponse{Grant: testGrant(r.Msg.GetId(), mycelv1.GrantState_GRANT_STATE_REVOKED)}), nil
}

func (f *fakeGrants) sharingResp() *mycelv1.GetNodeSharingResponse {
	return &mycelv1.GetNodeSharingResponse{
		Hub:       proto.Clone(f.sharing).(*mycelv1.SharingSettings),
		Local:     &mycelv1.SharingSettings{When: "idle", IdleAfterMinutes: 15},
		Effective: &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"gpu"}}, When: "idle", IdleAfterMinutes: 15, MaxJobs: 3},
	}
}

func (f *fakeGrants) GetNodeSharing(_ context.Context, r *connect.Request[mycelv1.GetNodeSharingRequest]) (*connect.Response[mycelv1.GetNodeSharingResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	if r.Msg.GetName() != "gpu-box" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("node "+r.Msg.GetName()))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(f.sharingResp()), nil
}

func (f *fakeGrants) SetNodeSharing(_ context.Context, r *connect.Request[mycelv1.SetNodeSharingRequest]) (*connect.Response[mycelv1.SetNodeSharingResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.sharing = r.Msg.GetSettings()
	return connect.NewResponse(&mycelv1.SetNodeSharingResponse{Sharing: f.sharingResp()}), nil
}

func (f *fakeGrants) FreezeChannel(_ context.Context, r *connect.Request[mycelv1.FreezeChannelRequest]) (*connect.Response[mycelv1.FreezeChannelResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.FreezeChannelResponse{}), nil
}

func (f *fakeGrants) UnfreezeChannel(_ context.Context, r *connect.Request[mycelv1.UnfreezeChannelRequest]) (*connect.Response[mycelv1.UnfreezeChannelResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.UnfreezeChannelResponse{}), nil
}

func (f *fakeGrants) FreezeAll(_ context.Context, r *connect.Request[mycelv1.FreezeAllRequest]) (*connect.Response[mycelv1.FreezeAllResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.FreezeAllResponse{}), nil
}

func (f *fakeGrants) UnfreezeAll(_ context.Context, r *connect.Request[mycelv1.UnfreezeAllRequest]) (*connect.Response[mycelv1.UnfreezeAllResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.UnfreezeAllResponse{}), nil
}

func (f *fakeGrants) FreezeStatus(_ context.Context, r *connect.Request[mycelv1.FreezeStatusRequest]) (*connect.Response[mycelv1.FreezeStatusResponse], error) {
	if err := f.hub.check(r.Header(), r.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.FreezeStatusResponse{AllFrozen: true, AllReason: "incident 7", AllSince: "2026-10-28T14:00:00Z", AllBy: "usr_1", FrozenChannels: []string{"#a", "#b"}}), nil
}

type cliCase struct {
	name     string
	args     []string
	wantCode int
	wantOut  []string
	notOut   []string
	wantErr  []string
	checkReq func(t *testing.T, m proto.Message)
	noReq    bool // the hub must not be called
	isJSON   bool
}

func runCases(t *testing.T, tests []cliCase) {
	t.Helper()
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
			if tt.isJSON && (!json.Valid([]byte(res.stdout)) || strings.Count(res.stdout, "\n") != 1) {
				t.Errorf("stdout is not one line of JSON: %q", res.stdout)
			}
			if tt.noReq && len(env.hub.headers()) != 0 {
				t.Errorf("hub was called: %v", env.hub.lastRequest())
			}
			if tt.checkReq != nil {
				tt.checkReq(t, env.hub.lastRequest())
			}
		})
	}
}

// protoEq checks m against want written as protojson.
func protoEq(want string) func(t *testing.T, m proto.Message) {
	return func(t *testing.T, m proto.Message) {
		t.Helper()
		w := proto.Clone(m)
		proto.Reset(w)
		if err := protojson.Unmarshal([]byte(want), w); err != nil {
			t.Fatalf("want %s: %v", want, err)
		}
		if !proto.Equal(m, w) {
			t.Errorf("request = %v, want %v", m, w)
		}
	}
}

func TestGrantCommands(t *testing.T) {
	runCases(t, []cliCase{
		{
			name: "grants ls", args: []string{"grants", "ls"},
			wantOut:  []string{"ID", "STATE", "RESOURCE", "SUBJECT", "EXPIRES", "grt_01", "active", "gpu device 0 exclusive", "agent:trainer@gpu-box", "in 2h", "grt_02", "pending", "for 2h", "train[2J model"},
			notOut:   []string{"\x1b"},
			checkReq: protoEq(`{}`),
		},
		{
			name: "grants ls filters", args: []string{"grants", "ls", "--pending", "--node", "gpu-box", "--subject", "trainer@gpu-box", "--all"},
			checkReq: protoEq(`{"pending_only":true,"node":"gpu-box","subject":"agent:trainer@gpu-box","include_finished":true}`),
		},
		{name: "grants ls json", args: []string{"--json", "grants", "ls"}, isJSON: true, wantOut: []string{`"grants":[`, `"id":"grt_01"`, `"state":"GRANT_STATE_ACTIVE"`}},
		{
			name: "grants get", args: []string{"grants", "get", "grt_01"},
			wantOut: []string{"State:", "active", "Resource:", "gpu device 0 exclusive", "Expires:", "2026-10-28T17:00:00Z (in 2h)", "Needs approval:", "yes"},
		},
		{name: "grants get unknown", args: []string{"grants", "get", "grt_x"}, wantCode: 1, wantErr: []string{"not found: grant grt_x"}},
		{
			name: "grants request gpu", args: []string{"grants", "request", "--kind", "gpu", "--subject", "trainer@gpu-box", "--device", "1", "--exclusive", "--duration", "2h", "--reason", "train", "--channel", "training-runs"},
			wantOut:  []string{"Grant grt_new is active until 2026-10-28T17:00:00Z (gpu device 1 exclusive on gpu-box)."},
			checkReq: protoEq(`{"spec":{"gpu":{"device":1,"exclusive":true}},"subject":"agent:trainer@gpu-box","duration":"2h","reason":"train","channel":"#training-runs"}`),
		},
		{
			name: "grants request ollama", args: []string{"grants", "request", "--kind", "ollama", "--subject", "job:job_01", "--model", "llama3,qwen", "--model", "phi", "--allow-pull"},
			wantOut:  []string{"Grant grt_new is pending approval: `mycel approve grt_new`"},
			checkReq: protoEq(`{"spec":{"ollama":{"models":["llama3","qwen","phi"],"allow_pull":true}},"subject":"job:job_01"}`),
		},
		{
			name: "grants request mount is read-only by default", args: []string{"grants", "request", "--kind", "mount", "--subject", "agent:a@b", "--host-path", "/data", "--guest-path", "/mnt/data"},
			checkReq: protoEq(`{"spec":{"mount":{"host_path":"/data","guest_path":"/mnt/data","read_only":true}},"subject":"agent:a@b"}`),
		},
		{
			name: "grants request mount --rw", args: []string{"grants", "request", "--kind", "mount", "--subject", "agent:a@b", "--host-path", "/data", "--guest-path", "/mnt/data", "--rw"},
			checkReq: protoEq(`{"spec":{"mount":{"host_path":"/data","guest_path":"/mnt/data"}},"subject":"agent:a@b"}`),
		},
		{name: "request --ro is gone", args: []string{"grants", "request", "--kind", "mount", "--subject", "agent:a@b", "--host-path", "/d", "--guest-path", "/m", "--ro"}, wantCode: 2, wantErr: []string{"unknown flag: --ro"}, noReq: true},
		{name: "request relative guest path", args: []string{"grants", "request", "--kind", "mount", "--subject", "agent:a@b", "--host-path", "/d", "--guest-path", "m/../x"}, wantCode: 2, wantErr: []string{"absolute, clean path"}, noReq: true},
		{name: "request bad egress rule", args: []string{"grants", "request", "--kind", "egress", "--subject", "agent:a@b", "--allow", "https://x.com/"}, wantCode: 2, wantErr: []string{"is not host, *.suffix or host:port"}, noReq: true},
		{name: "request direct channel", args: []string{"grants", "request", "--kind", "gpu", "--subject", "agent:a@b", "--channel", "dm:a:b"}, wantCode: 2, wantErr: []string{"must be a #channel name"}, noReq: true},
		{name: "request long reason", args: []string{"grants", "request", "--kind", "gpu", "--subject", "agent:a@b", "--reason", strings.Repeat("r", 1025)}, wantCode: 2, wantErr: []string{"more than the 1024 allowed"}, noReq: true},
		{
			name: "grants request docker-host", args: []string{"grants", "request", "--kind", "docker-host", "--subject", "agent:a@b", "--image", "python:3.12", "--image", "ghcr.io/x/"},
			checkReq: protoEq(`{"spec":{"docker_host":{"images":["python:3.12","ghcr.io/x/"]}},"subject":"agent:a@b"}`),
		},
		{
			name: "grants request egress json", args: []string{"--json", "grants", "request", "--kind", "egress", "--subject", "agent:a@b", "--allow", "pypi.org:443"}, isJSON: true,
			wantOut:  []string{`"id":"grt_new"`, `"state":"GRANT_STATE_PENDING"`},
			checkReq: protoEq(`{"spec":{"egress":{"allow":["pypi.org:443"]}},"subject":"agent:a@b"}`),
		},
		{name: "request without kind", args: []string{"grants", "request", "--subject", "agent:a@b"}, wantCode: 2, wantErr: []string{"--kind must be one of ollama, gpu, docker-host, mount, egress"}, noReq: true},
		{name: "request bad kind", args: []string{"grants", "request", "--kind", "tpu", "--subject", "agent:a@b"}, wantCode: 2, wantErr: []string{"--kind must be one of"}, noReq: true},
		{name: "request without subject", args: []string{"grants", "request", "--kind", "gpu"}, wantCode: 2, wantErr: []string{"--subject is required"}, noReq: true},
		{name: "request flag of another kind", args: []string{"grants", "request", "--kind", "gpu", "--subject", "agent:a@b", "--rw"}, wantCode: 2, wantErr: []string{"--rw does not apply to a gpu grant (its flags: --device, --exclusive)"}, noReq: true},
		{name: "request mount missing guest", args: []string{"grants", "request", "--kind", "mount", "--subject", "agent:a@b", "--host-path", "/x"}, wantCode: 2, wantErr: []string{"needs --host-path and --guest-path"}, noReq: true},
		{name: "request egress without allow", args: []string{"grants", "request", "--kind", "egress", "--subject", "agent:a@b"}, wantCode: 2, wantErr: []string{"at least one --allow"}, noReq: true},
		{name: "request docker without image", args: []string{"grants", "request", "--kind", "docker-host", "--subject", "agent:a@b"}, wantCode: 2, wantErr: []string{"at least one --image"}, noReq: true},
		{name: "request negative device", args: []string{"grants", "request", "--kind", "gpu", "--subject", "agent:a@b", "--device", "-1"}, wantCode: 2, wantErr: []string{"--device must not be negative"}, noReq: true},
		{name: "request bad duration", args: []string{"grants", "request", "--kind", "gpu", "--subject", "agent:a@b", "--duration", "48h"}, wantCode: 2, wantErr: []string{"24h"}, noReq: true},
		{
			name: "approve", args: []string{"approve", "grt_02", "--duration", "1h", "--note", "ok"},
			wantOut:  []string{"Approved grt_02: active until 2026-10-28T17:00:00Z."},
			checkReq: protoEq(`{"id":"grt_02","duration":"1h","note":"ok"}`),
		},
		{name: "grants approve alias", args: []string{"grants", "approve", "grt_02"}, wantOut: []string{"Approved grt_02"}, checkReq: protoEq(`{"id":"grt_02"}`)},
		{name: "approve bad duration", args: []string{"approve", "grt_02", "--duration", "x"}, wantCode: 2, wantErr: []string{"invalid duration"}, noReq: true},
		{name: "approve without id", args: []string{"approve"}, wantCode: 2, wantErr: []string{"expected 1 argument"}},
		{name: "deny", args: []string{"deny", "grt_02", "--note", "no"}, wantOut: []string{"Denied grt_02."}, checkReq: protoEq(`{"id":"grt_02","note":"no"}`)},
		{name: "grants deny json", args: []string{"--json", "grants", "deny", "grt_02"}, isJSON: true, wantOut: []string{`"state":"GRANT_STATE_DENIED"`}},
		{name: "revoke", args: []string{"grants", "revoke", "grt_01", "--note", "done"}, wantOut: []string{"Revoked grt_01."}, checkReq: protoEq(`{"id":"grt_01","note":"done"}`)},
	})
}

func TestFreezeCommands(t *testing.T) {
	runCases(t, []cliCase{
		{name: "freeze nothing", args: []string{"freeze"}, wantCode: 2, wantErr: []string{"give exactly one of --agent, --channel or --all"}, noReq: true},
		{name: "freeze agent and channel", args: []string{"freeze", "--agent", "a@b", "--channel", "x"}, wantCode: 2, wantErr: []string{"exactly one"}, noReq: true},
		{name: "freeze channel and all", args: []string{"freeze", "--channel", "x", "--all"}, wantCode: 2, wantErr: []string{"exactly one"}, noReq: true},
		{name: "freeze all three", args: []string{"freeze", "--agent", "a@b", "--channel", "x", "--all"}, wantCode: 2, wantErr: []string{"exactly one"}, noReq: true},
		{name: "freeze all=false", args: []string{"freeze", "--all=false"}, wantCode: 2, wantErr: []string{"selects nothing"}, noReq: true},
		{name: "freeze empty agent", args: []string{"freeze", "--agent", ""}, wantCode: 2, wantErr: []string{"--agent needs"}, noReq: true},
		{name: "unfreeze nothing", args: []string{"unfreeze"}, wantCode: 2, wantErr: []string{"exactly one"}, noReq: true},
		{name: "unfreeze two", args: []string{"unfreeze", "--agent", "a@b", "--all"}, wantCode: 2, wantErr: []string{"exactly one"}, noReq: true},
		{name: "unfreeze has no reason", args: []string{"unfreeze", "--all", "--reason", "x"}, wantCode: 2, wantErr: []string{"unknown flag: --reason"}, noReq: true},
		{name: "freeze stray argument", args: []string{"freeze", "everything"}, wantCode: 2, noReq: true},
		{
			name: "freeze agent", args: []string{"freeze", "--agent", "coder@mac", "--reason", "loop"},
			wantOut: []string{"Paused agent:coder@mac."}, checkReq: protoEq(`{"id":"agent:coder@mac","reason":"loop"}`),
		},
		{
			name: "freeze channel", args: []string{"freeze", "--channel", "backend", "--reason", "spam"},
			wantOut: []string{"Froze #backend"}, checkReq: protoEq(`{"channel":"#backend","reason":"spam"}`),
		},
		{
			name: "freeze all json", args: []string{"--json", "freeze", "--all", "--reason", "incident"}, isJSON: true,
			wantOut: []string{`{"frozen":true,"scope":"all","target":"network"}`}, checkReq: protoEq(`{"reason":"incident"}`),
		},
		{name: "freeze all", args: []string{"freeze", "--all"}, wantOut: []string{"Froze the network", "mycel unfreeze --all"}},
		{name: "unfreeze agent", args: []string{"unfreeze", "--agent", "agent:coder@mac"}, wantOut: []string{"Resumed agent:coder@mac."}, checkReq: protoEq(`{"id":"agent:coder@mac"}`)},
		{name: "unfreeze channel", args: []string{"unfreeze", "--channel", "#backend"}, wantOut: []string{"Unfroze #backend."}, checkReq: protoEq(`{"channel":"#backend"}`)},
		{name: "unfreeze all json", args: []string{"--json", "unfreeze", "--all"}, isJSON: true, wantOut: []string{`{"frozen":false,"scope":"all","target":"network"}`}},
		{
			name: "freeze status", args: []string{"freeze", "status"},
			wantOut: []string{"Network:", "frozen since 2026-10-28T14:00:00Z by usr_1", "Reason:", "incident 7", "Frozen channels:", "#a, #b"},
		},
		{name: "freeze status json", args: []string{"--json", "freeze", "status"}, isJSON: true, wantOut: []string{`"all_frozen":true`, `"frozen_channels":["#a","#b"]`}},
	})
}

func TestParseWindow(t *testing.T) {
	tests := []struct {
		in      string
		days    string
		wantErr string
	}{
		{in: "mon-fri=22:00-07:00", days: "mon,tue,wed,thu,fri"},
		{in: "sat,sun=00:00-24:00", days: "sat,sun"},
		{in: "SUN,sat=09:30-17:45", days: "sat,sun"},
		{in: "fri-mon=20:00-23:00", days: "mon,fri,sat,sun"},
		{in: "mon,wed-thu=08:00-09:00", days: "mon,wed,thu"},
		{in: "mon=23:59-00:00", days: "mon"},
		{in: "mon-fri", wantErr: "want days=HH:MM-HH:MM"},
		{in: "=22:00-07:00", wantErr: "want days=HH:MM-HH:MM"},
		{in: "mon=", wantErr: "want days=HH:MM-HH:MM"},
		{in: "monday=22:00-07:00", wantErr: "unknown day"},
		{in: "mon-xyz=22:00-07:00", wantErr: "unknown day"},
		{in: "mon,,tue=22:00-07:00", wantErr: "unknown day"},
		{in: "mon-mon=22:00-07:00", wantErr: "same day"},
		{in: "mon,mon=22:00-07:00", wantErr: "mon is listed twice"},
		{in: "mon-fri,wed=22:00-07:00", wantErr: "wed is listed twice"},
		{in: "mon=22:00", wantErr: "time range"},
		{in: "mon=25:00-07:00", wantErr: "out of range"},
		{in: "mon=22:60-07:00", wantErr: "not HH:MM"},
		{in: "mon=7:00-08:00", wantErr: "not HH:MM"},
		{in: "mon=aa:00-08:00", wantErr: "not HH:MM"},
		{in: "mon=+1:00-08:00", wantErr: "not HH:MM"},
		{in: "mon=01:+5-08:00", wantErr: "not HH:MM"},
		{in: "mon=01:00-0８:00", wantErr: "not HH:MM"},
		{in: "mon=00:00-24:00", days: "mon"},
		{in: "mon=-1:00-08:00", wantErr: "not HH:MM"},
		{in: "mon=24:00-08:00", wantErr: "cannot start at 24:00"},
		{in: "mon=08:00-24:01", wantErr: "out of range"},
		{in: "mon=08:00-08:00", wantErr: "empty"},
	}
	for _, tc := range tests {
		w, err := parseWindow(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseWindow(%q) = %v, %v; want error %q", tc.in, w, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWindow(%q): %v", tc.in, err)
			continue
		}
		if got := strings.Join(w.GetDays(), ","); got != tc.days {
			t.Errorf("parseWindow(%q) days = %s, want %s", tc.in, got, tc.days)
		}
		_, times, _ := strings.Cut(tc.in, "=")
		if w.GetFrom()+"-"+w.GetTo() != times {
			t.Errorf("parseWindow(%q) = %s-%s", tc.in, w.GetFrom(), w.GetTo())
		}
	}
}

func TestNodesShare(t *testing.T) {
	runCases(t, []cliCase{
		{
			name: "show without flags", args: []string{"nodes", "share", "gpu-box"},
			wantOut:  []string{"SETTING", "HUB", "LOCAL", "EFFECTIVE", "Offer", "docker,gpu", "When", "schedule", "idle", "Schedule", "mon,tue,wed,thu,fri=22:00-07:00", "Local first", "yes", "Max jobs", "3", "default", "Idle after", "10m", "15m"},
			checkReq: protoEq(`{"name":"gpu-box"}`),
		},
		{
			name: "only max-jobs changes, the rest is kept", args: []string{"nodes", "share", "gpu-box", "--max-jobs", "5"},
			checkReq: protoEq(`{"name":"gpu-box","settings":{"offer":{"names":["docker","gpu"]},"when":"schedule",
				"schedule":[{"days":["mon","tue","wed","thu","fri"],"from":"22:00","to":"07:00"}],"local_first":true,"max_jobs":5,"idle_after_minutes":10}}`),
		},
		{
			name: "offer, local-first=false, idle", args: []string{"nodes", "share", "gpu-box", "--offer", "ollama", "--local-first=false", "--when", "idle", "--idle-after", "30m"},
			checkReq: protoEq(`{"name":"gpu-box","settings":{"offer":{"names":["ollama"]},"when":"idle","max_jobs":3,"idle_after_minutes":30}}`),
		},
		{
			name: "windows replace the schedule", args: []string{"nodes", "share", "gpu-box", "--window", "sat,sun=00:00-24:00", "--window", "mon-fri=22:00-07:00"},
			checkReq: protoEq(`{"name":"gpu-box","settings":{"offer":{"names":["docker","gpu"]},"when":"schedule",
				"schedule":[{"days":["sat","sun"],"from":"00:00","to":"24:00"},{"days":["mon","tue","wed","thu","fri"],"from":"22:00","to":"07:00"}],"local_first":true,"max_jobs":3,"idle_after_minutes":10}}`),
		},
		{
			name: "offer nothing", args: []string{"--json", "nodes", "share", "gpu-box", "--offer", ""}, isJSON: true,
			wantOut:  []string{`"hub":{"offer":{"names":[]}`},
			checkReq: protoEq(`{"name":"gpu-box","settings":{"offer":{},"when":"schedule","schedule":[{"days":["mon","tue","wed","thu","fri"],"from":"22:00","to":"07:00"}],"local_first":true,"max_jobs":3,"idle_after_minutes":10}}`),
		},
		{name: "bad when", args: []string{"nodes", "share", "gpu-box", "--when", "sometimes"}, wantCode: 2, wantErr: []string{"invalid --when"}},
		{name: "window with when always", args: []string{"nodes", "share", "gpu-box", "--when", "always", "--window", "mon=08:00-09:00"}, wantCode: 2, wantErr: []string{"--window needs --when schedule"}},
		{name: "bad window", args: []string{"nodes", "share", "gpu-box", "--window", "noday=08:00-09:00"}, wantCode: 2, wantErr: []string{"unknown day"}},
		{name: "bad idle-after", args: []string{"nodes", "share", "gpu-box", "--idle-after", "90s"}, wantCode: 2, wantErr: []string{"whole minutes"}},
		{name: "negative max-jobs", args: []string{"nodes", "share", "gpu-box", "--max-jobs", "-1"}, wantCode: 2, wantErr: []string{"must not be negative"}},
		{name: "unknown node", args: []string{"nodes", "share", "nope", "--max-jobs", "1"}, wantCode: 1, wantErr: []string{"not found: node nope"}},
	})
}

func TestApplyShareScheduleNeedsWindow(t *testing.T) {
	f := &shareFlags{when: "schedule"}
	changed := func(n string) bool { return n == "when" }
	if _, err := applyShare(&mycelv1.SharingSettings{When: "always"}, f, changed); err == nil || !strings.Contains(err.Error(), "at least one --window") {
		t.Errorf("err = %v", err)
	}
	base := &mycelv1.SharingSettings{When: "always", MaxJobs: 2}
	got, err := applyShare(base, &shareFlags{maxJobs: 4}, func(n string) bool { return n == "max-jobs" })
	if err != nil || got.GetMaxJobs() != 4 || got.GetWhen() != "always" || base.GetMaxJobs() != 2 {
		t.Errorf("applyShare = %v, %v; base %v", got, err, base)
	}
}

// localControls is an httptest fake of the phase-5 local API endpoints.
type localControls struct {
	mu    sync.Mutex
	reqs  []string
	put   string
	local string // the "local" settings in GET /api/local/sharing
}

func newLocalControls(t *testing.T, token string) (*localControls, *httptest.Server) {
	t.Helper()
	f := &localControls{local: `{"offer":{"names":["docker","gpu"]}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad token"}`))
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/local/sharing":
			f.mu.Lock()
			local := f.local
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"hub":{"offer":{"names":["docker","gpu","ollama"]},"when":"always","max_jobs":4},
				"local":` + local + `,
				"effective":{"offer":{"names":["docker","gpu"]},"when":"always","max_jobs":4,"local_first":true}}`))
		case "PUT /api/local/sharing":
			f.mu.Lock()
			f.put = string(b)
			f.mu.Unlock()
			var s mycelv1.SharingSettings
			if err := protojson.Unmarshal(b, &s); err != nil || s.GetMaxJobs() > 4 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"max_jobs may only be lowered on this machine"}`))
				return
			}
			_, _ = w.Write(b)
		case "GET /api/local/grants":
			_, _ = w.Write([]byte(`{"grants":[{"id":"grt_01","spec":{"ollama":{"models":["llama3"]}},"state":"GRANT_STATE_ACTIVE","subject":"agent:x@y","node":"mac","expires_at":"2026-10-28T15:30:00Z"}]}`))
		case "DELETE /api/local/grants/grt_01":
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/local/freeze":
			_, _ = w.Write([]byte(`{"frozen":true,"scope":"channel:#backend","reason":"spam"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found: no grant"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestNodeLocalCommands(t *testing.T) {
	const tok = "tok-local"
	t.Setenv(config.EnvHome, t.TempDir())
	writeLocalToken(t, tok)
	f, srv := newLocalControls(t, tok)
	la := []string{"--local-addr", srv.URL}
	run := func(args ...string) result { return runCLI(t, "", append(args, la...)...) }

	res := run("node", "share")
	for _, w := range []string{"HUB", "LOCAL", "EFFECTIVE", "docker,gpu,ollama", "always", "4"} {
		if res.code != 0 || !strings.Contains(res.stdout, w) {
			t.Errorf("node share missing %q: %+v", w, res)
		}
	}
	if f.put != "" {
		t.Errorf("node share without flags wrote %s", f.put)
	}

	res = run("node", "share", "--offer", "gpu", "--max-jobs", "2")
	if res.code != 0 || !strings.Contains(res.stdout, "This machine now shares:") || !strings.Contains(res.stdout, "Offer:") {
		t.Fatalf("node share --offer: %+v", res)
	}
	var put mycelv1.SharingSettings
	if err := protojson.Unmarshal([]byte(f.put), &put); err != nil {
		t.Fatal(err)
	}
	// Read-modify-write starts from the local ceiling when one is set.
	want := &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"gpu"}}, MaxJobs: 2}
	if !proto.Equal(&put, want) {
		t.Errorf("PUT body = %v, want %v", &put, want)
	}

	// Without a local ceiling it starts from the effective settings.
	f.mu.Lock()
	f.local = `{}`
	f.mu.Unlock()
	if res := run("node", "share", "--max-jobs", "1"); res.code != 0 {
		t.Fatalf("node share from effective: %+v", res)
	}
	put.Reset()
	if err := protojson.Unmarshal([]byte(f.put), &put); err != nil {
		t.Fatal(err)
	}
	want = &mycelv1.SharingSettings{Offer: &mycelv1.Capabilities{Names: []string{"docker", "gpu"}}, When: "always", MaxJobs: 1, LocalFirst: true}
	if !proto.Equal(&put, want) {
		t.Errorf("PUT body = %v, want %v", &put, want)
	}

	res = run("node", "share", "--max-jobs", "9")
	if res.code != 1 || !strings.Contains(res.stderr, "may only be lowered") {
		t.Errorf("widening: %+v", res)
	}
	res = run("node", "share", "--when", "never")
	if res.code != 2 {
		t.Errorf("bad --when: %+v", res)
	}

	res = run("node", "grants", "ls")
	for _, w := range []string{"grt_01", "active", "ollama models llama3", "agent:x@y", "in 30m"} {
		if res.code != 0 || !strings.Contains(res.stdout, w) {
			t.Errorf("node grants ls missing %q: %+v", w, res)
		}
	}
	res = run("--json", "node", "grants", "ls")
	if !json.Valid([]byte(res.stdout)) || !strings.Contains(res.stdout, `"id":"grt_01"`) {
		t.Errorf("node grants ls --json: %+v", res)
	}
	res = run("node", "grants", "revoke", "grt_01")
	if res.code != 0 || !strings.Contains(res.stdout, "Revoked grt_01 on this machine.") {
		t.Errorf("revoke: %+v", res)
	}
	res = run("node", "grants", "revoke", "grt_09")
	if res.code != 1 || !strings.Contains(res.stderr, "no grant") {
		t.Errorf("revoke unknown: %+v", res)
	}
	res = run("node", "freeze", "status")
	if res.code != 0 || !strings.Contains(res.stdout, "This machine is frozen (scope channel:#backend): spam.") {
		t.Errorf("freeze status: %+v", res)
	}
	res = run("--json", "node", "freeze", "status")
	if res.stdout != `{"frozen":true,"scope":"channel:#backend","reason":"spam"}`+"\n" {
		t.Errorf("freeze status --json = %q", res.stdout)
	}

	f.mu.Lock()
	reqs := strings.Join(f.reqs, "\n")
	f.mu.Unlock()
	for _, w := range []string{"GET /api/local/sharing", "PUT /api/local/sharing", "GET /api/local/grants", "DELETE /api/local/grants/grt_01", "GET /api/local/freeze"} {
		if !strings.Contains(reqs, w) {
			t.Errorf("requests missing %s:\n%s", w, reqs)
		}
	}
}

func TestNodeKeepsNodesSubcommands(t *testing.T) {
	env := newTestEnv(t)
	env.writeConfig(t)
	res := runCLI(t, "", "node", "ls")
	if res.code != 0 || !strings.Contains(res.stdout, "NAME") {
		t.Errorf("node ls: %+v", res)
	}
	res = runCLI(t, "", "node")
	for _, w := range []string{"share", "grants", "freeze"} {
		if !strings.Contains(res.stdout, w) {
			t.Errorf("node help missing %s: %s", w, res.stdout)
		}
	}
}

func TestUntil(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	for ts, want := range map[string]string{
		"":                     "-",
		"2026-10-28T17:00:00Z": "in 2h",
		"2026-10-28T15:30:20Z": "in 30m",
		"2026-10-28T15:00:20Z": "in <1m",
		"2026-10-28T14:00:00Z": "1h ago",
		"2026-10-28T16:05:59Z": "in 1h5m",
		"bogus":                "bogus",
	} {
		if got := until(ts, now); got != want {
			t.Errorf("until(%q) = %q, want %q", ts, got, want)
		}
	}
}
