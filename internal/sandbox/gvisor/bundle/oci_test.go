package bundle

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

func TestOCIConfigLimits(t *testing.T) {
	tests := []struct {
		name    string
		limits  sandbox.Limits
		rlimits []Rlimit
		res     *Resources
	}{
		{
			name:    "no limits",
			rlimits: []Rlimit{{Type: "RLIMIT_NOFILE", Hard: NofileLimit, Soft: NofileLimit}},
			res:     &Resources{Devices: []DeviceCgroup{{Allow: false, Access: "rwm"}}},
		},
		{
			name:   "all limits",
			limits: sandbox.Limits{CPUs: 2, MemoryBytes: 4 << 30, MaxProcesses: 512},
			rlimits: []Rlimit{
				{Type: "RLIMIT_NOFILE", Hard: NofileLimit, Soft: NofileLimit},
				{Type: "RLIMIT_NPROC", Hard: 512, Soft: 512},
			},
			res: &Resources{
				Devices: []DeviceCgroup{{Allow: false, Access: "rwm"}},
				Memory:  &Memory{Limit: 4 << 30, Swap: 4 << 30},
				CPU:     &CPU{Quota: 200000, Period: 100000},
				Pids:    &Pids{Limit: 512},
			},
		},
		{
			name:   "processes only",
			limits: sandbox.Limits{MaxProcesses: 64},
			rlimits: []Rlimit{
				{Type: "RLIMIT_NOFILE", Hard: NofileLimit, Soft: NofileLimit},
				{Type: "RLIMIT_NPROC", Hard: 64, Soft: 64},
			},
			res: &Resources{Devices: []DeviceCgroup{{Allow: false, Access: "rwm"}}, Pids: &Pids{Limit: 64}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := OCIConfig(ConfigInput{ // #nosec G101 -- test credential
				Spec:   sandbox.Spec{ID: "sbx:1", Limits: tt.limits},
				Rootfs: "/s/rootfs", Workspace: "/s/workspace", CgroupsPath: "/mycel/sbx-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Process.Rlimits, tt.rlimits) {
				t.Errorf("rlimits = %+v, want %+v", c.Process.Rlimits, tt.rlimits)
			}
			if !reflect.DeepEqual(c.Linux.Resources, tt.res) {
				t.Errorf("resources = %+v, want %+v", c.Linux.Resources, tt.res)
			}
			if c.Linux.CgroupsPath != "/mycel/sbx-1" {
				t.Errorf("cgroupsPath = %q", c.Linux.CgroupsPath)
			}
			wantTmp := "size=536870912"
			if tt.limits.MemoryBytes > 0 {
				wantTmp = fmt.Sprintf("size=%d", tt.limits.MemoryBytes/2)
			}
			var tmp []string
			for _, m := range c.Mounts {
				if m.Destination == "/tmp" {
					tmp = m.Options
				}
			}
			if !contains(tmp, wantTmp) {
				t.Errorf("/tmp options %v, want %s", tmp, wantTmp)
			}
		})
	}
}

func TestOCIConfigShape(t *testing.T) {
	spec := sandbox.Spec{
		ID:  "sbx:01ABC",
		Env: map[string]string{"B": "2", "A": "1", "HTTPS_PROXY": "http://evil:1", "PATH": "/opt/bin"},
	}
	c, err := OCIConfig(ConfigInput{ // #nosec G101 -- test credential
		Spec: spec, Rootfs: "/var/lib/mycel/sbx/sbx-01ABC/rootfs", Workspace: "/var/lib/mycel/sbx/sbx-01ABC/workspace",
		CgroupsPath: "/mycel/sbx-01ABC", Netns: "/var/run/netns/mycel-x", ProxyURL: "http://sbx:tok@10.237.0.1:3128", // #nosec G101 -- test credential
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Process.Args; !reflect.DeepEqual(got, DefaultInit) {
		t.Errorf("args = %v", got)
	}
	if c.Process.Cwd != "/workspace" {
		t.Errorf("cwd = %q", c.Process.Cwd)
	}
	if !c.Process.NoNewPrivileges {
		t.Error("noNewPrivileges not set")
	}
	if c.Process.Terminal {
		t.Error("terminal set")
	}
	wantEnv := []string{
		"A=1", "B=2", "HOME=/tmp", "PATH=/opt/bin",
		"HTTP_PROXY=http://sbx:tok@10.237.0.1:3128", "HTTPS_PROXY=http://sbx:tok@10.237.0.1:3128",
		"http_proxy=http://sbx:tok@10.237.0.1:3128", "https_proxy=http://sbx:tok@10.237.0.1:3128",
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
	if !reflect.DeepEqual(c.Process.Env, wantEnv) {
		t.Errorf("env = %v\nwant %v", c.Process.Env, wantEnv)
	}
	if c.Root.Path != "/var/lib/mycel/sbx/sbx-01ABC/rootfs" || c.Root.Readonly {
		t.Errorf("root = %+v", c.Root)
	}
	var binds []OCIMount
	for _, m := range c.Mounts {
		if m.Type == "bind" || strings.HasPrefix(m.Source, "/") {
			binds = append(binds, m)
		}
	}
	if len(binds) != 1 || binds[0].Destination != "/workspace" || binds[0].Source != "/var/lib/mycel/sbx/sbx-01ABC/workspace" {
		t.Errorf("host mounts = %+v, want exactly the workspace bind", binds)
	}
	if len(c.Linux.Devices) != 0 {
		t.Errorf("devices = %+v", c.Linux.Devices)
	}
	if !reflect.DeepEqual(c.Linux.MaskedPaths, MaskedPaths) || !reflect.DeepEqual(c.Linux.ReadonlyPaths, ReadonlyPaths) {
		t.Error("masked/readonly paths differ from the OCI defaults")
	}
	var netns string
	for _, ns := range c.Linux.Namespaces {
		if ns.Type == "network" {
			netns = ns.Path
		}
	}
	if netns != "/var/run/netns/mycel-x" {
		t.Errorf("network namespace = %q", netns)
	}
	wantCaps := []string{"CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETGID", "CAP_SETUID", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL"}
	if !reflect.DeepEqual(c.Process.Capabilities.Bounding, wantCaps) {
		t.Errorf("bounding caps %v", c.Process.Capabilities.Bounding)
	}
	if c.Process.User != (User{UID: 1000, GID: 1000}) {
		t.Errorf("user %+v", c.Process.User)
	}
	if c.Linux.CgroupsPath != "/mycel/sbx-01ABC" {
		t.Errorf("cgroupsPath %q", c.Linux.CgroupsPath)
	}
	root := User{}
	rc, err := OCIConfig(ConfigInput{Spec: sandbox.Spec{ID: "x"}, Rootfs: "/r", Workspace: "/w", CgroupsPath: "/mycel/x", User: &root})
	if err != nil || rc.Process.User != root || !contains(rc.Process.Env, "HOME=/root") {
		t.Errorf("configurable user: %+v %v", rc.Process, err)
	}
	if _, err := OCIConfig(ConfigInput{Spec: sandbox.Spec{ID: "x"}, Rootfs: "/r", Workspace: "/w"}); err == nil {
		t.Error("missing cgroups path accepted")
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	linux := generic["linux"].(map[string]any)
	if d, ok := linux["devices"].([]any); !ok || len(d) != 0 {
		t.Errorf("linux.devices in JSON = %v, want []", linux["devices"])
	}
	if generic["process"].(map[string]any)["noNewPrivileges"] != true {
		t.Error("noNewPrivileges missing from JSON")
	}
}

func TestOCIConfigNoEgress(t *testing.T) {
	c, err := OCIConfig(ConfigInput{Spec: sandbox.Spec{ID: "x"}, Rootfs: "/r", Workspace: "/w", CgroupsPath: "/mycel/x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Process.Env {
		if strings.Contains(strings.ToLower(e), "proxy") {
			t.Errorf("proxy variable without egress: %s", e)
		}
	}
}

func TestOCIConfigRefusesMounts(t *testing.T) {
	_, err := OCIConfig(ConfigInput{
		Spec:   sandbox.Spec{ID: "x", Mounts: []sandbox.Mount{{HostPath: "/data", GuestPath: "/data"}}},
		Rootfs: "/r", Workspace: "/w", CgroupsPath: "/mycel/x",
	})
	if err == nil {
		t.Fatal("mount grant accepted")
	}
}

func TestRewriteProxyURL(t *testing.T) {
	tests := []struct {
		in, host, want string
		ok             bool
	}{
		{"http://sbx-1:secret@127.0.0.1:3128", "10.237.0.1", "http://sbx-1:secret@10.237.0.1:3128", true},
		{"http://sbx-1:secret@0.0.0.0:41000", "172.22.16.1", "http://sbx-1:secret@172.22.16.1:41000", true},
		{"http://localhost:8080", "fd00::1", "http://[fd00::1]:8080", true},
		{"http://127.0.0.1", "10.0.0.1", "", false},
		{"127.0.0.1:3128", "10.0.0.1", "", false},
		{"://bad", "10.0.0.1", "", false},
	}
	for _, tt := range tests {
		got, err := RewriteProxyURL(tt.in, tt.host)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("RewriteProxyURL(%q, %q) = %q, %v; want %q ok=%v", tt.in, tt.host, got, err, tt.want, tt.ok)
		}
	}
}
