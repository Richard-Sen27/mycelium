package profiles

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAllEmbedded(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.GetName())
	}
	if got := strings.Join(names, ","); got != "default,dev,fake" {
		t.Fatalf("names = %s", got)
	}
	dev, err := Get("dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.GetLimits().GetWallClock() != "2h0m0s" || dev.GetLimits().GetCpus() != 4 {
		t.Fatalf("dev limits = %v", dev.GetLimits())
	}
	if len(dev.GetBrokers()) != 2 || len(dev.GetEgress().GetAllow()) == 0 {
		t.Fatalf("dev = %v", dev)
	}
	// Callers get copies.
	dev.Name = "mutated"
	again, _ := Get("dev")
	if again.GetName() != "dev" {
		t.Fatal("Get returned shared state")
	}
	if _, err := Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(nope) = %v", err)
	}
}

func TestIsFake(t *testing.T) {
	for name, want := range map[string]bool{"default": false, "dev": false, "fake": true} {
		p, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if IsFake(p) != want {
			t.Errorf("IsFake(%s) = %v", name, !want)
		}
	}
	if IsFake(nil) {
		t.Error("IsFake(nil)")
	}
}

const base = `name: x
version: 1
backends: [gvisor]
image: debian
`

func TestParse(t *testing.T) {
	cases := []struct {
		name, doc, err string
	}{
		{"ok", base, ""},
		{"ok full", base + "limits: {cpus: 1, wall_clock: 90m}\negress: {allow: [a.com, '*.b.org', 'c.io:8443', '10.0.0.1', '10.0.0.1:80', '[2001:db8::1]:443']}\nbrokers: [ollama, gpu]\n", ""},
		{"bad name", strings.Replace(base, "name: x", "name: X_y", 1), "name"},
		{"long name", strings.Replace(base, "name: x", "name: "+strings.Repeat("a", 33), 1), "name"},
		{"version 0", strings.Replace(base, "version: 1", "version: 0", 1), "version"},
		{"no backends", strings.Replace(base, "backends: [gvisor]", "backends: []", 1), "backends"},
		{"bad backend", strings.Replace(base, "[gvisor]", "[docker]", 1), "unknown backend"},
		{"dup backend", strings.Replace(base, "[gvisor]", "[gvisor, gvisor]", 1), "twice"},
		{"no image", strings.Replace(base, "image: debian", "image: ''", 1), "image"},
		{"bad duration", base + "limits: {wall_clock: soon}\n", "wall_clock"},
		{"negative duration", base + "limits: {wall_clock: -1h}\n", "wall_clock"},
		{"negative limit", base + "limits: {cpus: -1}\n", "negative"},
		{"bad broker", base + "brokers: [docker]\n", "unknown broker"},
		{"bad rule", base + "egress: {allow: ['http://a.com']}\n", "egress"},
		{"bad port", base + "egress: {allow: ['a.com:0']}\n", "port"},
		{"bad wildcard", base + "egress: {allow: ['*.com']}\n", "wildcard"},
		{"inner wildcard", base + "egress: {allow: ['a.*.com']}\n", "label"},
		{"unknown key", base + "colour: red\n", "decode"},
		{"mount refused", base + "mounts: [{host_path: /etc, guest_path: /data}]\n", "mount refused"},
		{"mount relative", base + "mounts: [{host_path: data, guest_path: /data}]\n", "mount refused"},
		{"fake with others", strings.Replace(base, "[gvisor]", "[fake, gvisor]", 1), "only backend"},
		{"fake alone", strings.Replace(base, "[gvisor]", "[fake]", 1), ""},
		{"approval ok", base + "requires_approval: [rw-mount, gpu, egress-outside-allowlist]\n", ""},
		{"approval unknown", base + "requires_approval: [always]\n", "requires_approval"},
		{"trailing document", base + "---\nname: y\n", "one YAML document"},
		{"image registry port digest", strings.Replace(base, "image: debian", "image: 'registry.example.com:5000/team/app:v1.2@sha256:"+strings.Repeat("a", 64)+"'", 1), ""},
		{"image uppercase repo", strings.Replace(base, "image: debian", "image: library/Debian", 1), "image"},
		{"image whitespace", strings.Replace(base, "image: debian", "image: 'debian bookworm'", 1), "image"},
		{"image leading dash", strings.Replace(base, "image: debian", "image: '-debian'", 1), "image"},
		{"image bad tag", strings.Replace(base, "image: debian", "image: 'debian:'", 1), "image"},
		{"egress loopback", base + "egress: {allow: ['127.0.0.1']}\n", "loopback"},
		{"egress v6 loopback", base + "egress: {allow: ['[::1]']}\n", "loopback"},
		{"egress link-local", base + "egress: {allow: ['169.254.169.254']}\n", "link-local"},
		{"egress unspecified", base + "egress: {allow: ['0.0.0.0:80']}\n", "unspecified"},
		{"egress multicast", base + "egress: {allow: ['224.0.0.1']}\n", "multicast"},
		{"egress localhost", base + "egress: {allow: ['localhost:8080']}\n", "localhost"},
		{"egress sub localhost", base + "egress: {allow: ['*.app.localhost']}\n", "localhost"},
		{"egress numeric name", base + "egress: {allow: ['127.1']}\n", "numeric"},
		{"egress hex name", base + "egress: {allow: ['0x7f000001']}\n", "numeric"},
		{"egress wildcard numeric", base + "egress: {allow: ['*.0.1']}\n", "numeric"},
		{"egress uppercase", base + "egress: {allow: ['GitHub.com']}\n", "label"},
		{"egress bare v6", base + "egress: {allow: ['2001:db8::1']}\n", "brackets"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if c.err == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want containing %q", err, c.err)
			}
		})
	}
}

func TestValidateEgressRuleGrammar(t *testing.T) {
	for rule, ok := range map[string]bool{
		"pypi.org": true, "*.github.com": true, "api.anthropic.com:443": true, "1.2.3.4": true,
		"1.2.3.4:8080": true, "[2001:db8::1]": true, "[2001:db8::1]:443": true, "[::1]": true,
		"": false, "*.com": false, "a.*.com": false, "Pypi.org": false, "a.com:0": false,
		"a.com:65536": false, "a.com:+80": false, "::1": false, "[1.2.3.4]": false, "[::1]x": false,
		"[::1": false, "-a.com": false, "a..com": false, "http://a.com": false, "a.com/x": false,
	} {
		if err := ValidateEgressRule(rule); (err == nil) != ok {
			t.Errorf("ValidateEgressRule(%q) = %v", rule, err)
		}
	}
}

func TestLoadFSRules(t *testing.T) {
	fsys := fstest.MapFS{"y.yaml": {Data: []byte(base)}}
	if _, err := loadFS(fsys); err == nil || !strings.Contains(err.Error(), "file name") {
		t.Fatalf("mismatched name: %v", err)
	}
	fsys = fstest.MapFS{"x.yaml": {Data: []byte(base)}}
	got, err := loadFS(fsys)
	if err != nil || len(got) != 1 {
		t.Fatalf("loadFS = %v, %v", got, err)
	}
}
