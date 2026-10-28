package bundle

import (
	"archive/tar"
	"bytes"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestNetArgvNoDNS(t *testing.T) {
	n := testNet(t, FirewallNFT, "10.237.0.13:3128")
	n.DNS = netip.Addr{}
	for _, l := range joinAll(n.SetupArgv()) {
		if strings.Contains(l, "dport 53") {
			t.Errorf("DNS rule without AllowDNS: %s", l)
		}
	}
	n.Firewall = FirewallIPTables
	for _, l := range joinAll(n.SetupArgv()) {
		if strings.Contains(l, "--dport 53") {
			t.Errorf("DNS rule without AllowDNS: %s", l)
		}
	}
}

func TestSlotAllocatorSkip(t *testing.T) {
	a := NewSlotAllocator(netip.MustParsePrefix("10.0.0.0/28")) // four slots
	taken := map[int]bool{0: true, 1: true, 2: true}
	s, err := a.Get("x", func(slot int) bool { return taken[slot] })
	if err != nil || s != 3 {
		t.Fatalf("slot %d %v", s, err)
	}
	if _, err := a.Get("y", func(slot int) bool { return taken[slot] }); err == nil {
		t.Error("assigned slots handed out")
	}
}

func TestHostRulesNFT(t *testing.T) {
	want := `table inet mycel
delete table inet mycel
table inet mycel {
	chain input {
		type filter hook input priority 0; policy accept;
		tcp dport 3128 iifname != "lo" iifname != "myh*" drop
	}
	chain output {
		type filter hook output priority 0; policy accept;
		oifname "myh*" ct state established,related accept
		oifname "myh*" drop
	}
}
`
	if got := HostRulesNFT(3128); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := HostRulesNFT(0); strings.Contains(got, "dport") || !strings.Contains(got, `oifname "myh*" drop`) {
		t.Errorf("no guard:\n%s", got)
	}
}

func TestHostRulesIPTables(t *testing.T) {
	want := []string{
		"iptables -w -N MYCEL-HOST",
		"iptables -w -A MYCEL-HOST -i lo -j RETURN",
		"iptables -w -A MYCEL-HOST -i myh+ -j RETURN",
		"iptables -w -A MYCEL-HOST -j DROP",
		"iptables -w -I INPUT -p tcp --dport 3128 -j MYCEL-HOST",
		"iptables -w -I OUTPUT -o myh+ -m conntrack ! --ctstate ESTABLISHED,RELATED -j DROP",
		"ip6tables -w -I OUTPUT -o myh+ -j DROP",
	}
	if got := joinAll(HostRulesIPTables(3128)); !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s", strings.Join(got, "\n"))
	}
	if got := joinAll(HostRulesIPTables(0)); !reflect.DeepEqual(got, want[5:]) {
		t.Errorf("no guard: %v", got)
	}
}

func TestRulesToDelete(t *testing.T) {
	out := []byte(`-P INPUT ACCEPT
-N MYCEL-HOST
-N MYCEL-0123456789
-N MYCEL-abcdefabcd
-A INPUT -p tcp -m tcp --dport 3128 -j MYCEL-HOST
-A INPUT -i myh0123456789 -j MYCEL-0123456789
-A INPUT -i myhabcdefabcd -j MYCEL-abcdefabcd
-A MYCEL-0123456789 -s 10.237.0.14/32 -d 10.237.0.13/32 -p tcp -m tcp --dport 3128 -j ACCEPT
-A OUTPUT -o myh+ -m conntrack ! --ctstate RELATED,ESTABLISHED -j DROP
`)
	dead := func(l string) bool { return sandboxRuleShort(l) == "0123456789" }
	want := []string{
		"iptables -w -D INPUT -i myh0123456789 -j MYCEL-0123456789",
		"iptables -w -D MYCEL-0123456789 -s 10.237.0.14/32 -d 10.237.0.13/32 -p tcp -m tcp --dport 3128 -j ACCEPT",
		"iptables -w -F MYCEL-0123456789",
		"iptables -w -X MYCEL-0123456789",
	}
	if got := joinAll(RulesToDelete("iptables", "", out, dead)); !reflect.DeepEqual(got, want) {
		t.Errorf("sandbox rules:\n%s", strings.Join(got, "\n"))
	}
	wantHost := []string{
		"ip6tables -w -t filter -D INPUT -p tcp -m tcp --dport 3128 -j MYCEL-HOST",
		"ip6tables -w -t filter -D OUTPUT -o myh+ -m conntrack ! --ctstate RELATED,ESTABLISHED -j DROP",
		"ip6tables -w -t filter -F MYCEL-HOST",
		"ip6tables -w -t filter -X MYCEL-HOST",
	}
	if got := joinAll(RulesToDelete("ip6tables", "filter", out, IsHostRule)); !reflect.DeepEqual(got, wantHost) {
		t.Errorf("host rules:\n%s", strings.Join(got, "\n"))
	}
}

func TestGCParsers(t *testing.T) {
	if got := ParseNetns([]byte("mycel-0123456789 (id: 3)\nmycel-bad\nother\nmycel-abcdefabcd\n")); !reflect.DeepEqual(got, []string{"0123456789", "abcdefabcd"}) {
		t.Errorf("netns %v", got)
	}
	links := "1: lo: <LOOPBACK,UP> mtu 65536\n12: myh0123456789@if11: <BROADCAST> mtu 1500\n13: myhost: <UP>\n14: eth0: <UP>\n"
	if got := ParseLinks([]byte(links)); !reflect.DeepEqual(got, []string{"0123456789"}) {
		t.Errorf("links %v", got)
	}
	tables := "table inet filter\ntable inet mycel\ntable inet mycel_0123456789\ntable ip mycel_abcdefabcd\n"
	if got := ParseNFTTables([]byte(tables)); !reflect.DeepEqual(got, []string{"0123456789"}) {
		t.Errorf("tables %v", got)
	}
	addrs := "1: lo    inet 127.0.0.1/8 scope host lo\n12: myh0123456789    inet 10.237.0.13/30 scope global myh0123456789\n"
	got := ParseAddrs([]byte(addrs))
	if !got[netip.MustParseAddr("10.237.0.13")] || !got[netip.MustParseAddr("127.0.0.1")] || len(got) != 2 {
		t.Errorf("addrs %v", got)
	}
}

type tarEntry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func mkTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFilterRootfs(t *testing.T) {
	in := mkTar(t, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", typ: tar.TypeReg, mode: 0o644, body: "root:x:0:0"},
		{name: "usr/bin/sudo", typ: tar.TypeReg, mode: 0o4755, body: "elf"},
		{name: "usr/bin/wall", typ: tar.TypeReg, mode: 0o2755, body: "elf"},
		{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
		{name: "dev/sda", typ: tar.TypeBlock, mode: 0o660},
		{name: "run/fifo", typ: tar.TypeFifo, mode: 0o600},
		{name: "/abs", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "../escape", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "a/../../escape", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "evil", link: "/", typ: tar.TypeSymlink, mode: 0o777},
		{name: "evil/etc/shadow", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "bin", link: "usr/bin", typ: tar.TypeSymlink, mode: 0o777},
		{name: "hard-ok", link: "etc/passwd", typ: tar.TypeLink},
		{name: "hard-abs", link: "/etc/shadow", typ: tar.TypeLink},
		{name: "hard-up", link: "../x", typ: tar.TypeLink},
		{name: "hard-missing", link: "nowhere", typ: tar.TypeLink},
	})
	var out bytes.Buffer
	st, err := FilterRootfs(bytes.NewReader(in), &out)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	got := map[string]*tar.Header{}
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[strings.TrimSuffix(h.Name, "/")] = h
		names = append(names, h.Name)
	}
	wantNames := []string{"etc/", "etc/passwd", "usr/bin/sudo", "usr/bin/wall", "evil", "bin", "hard-ok"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("kept %v\nwant %v", names, wantNames)
	}
	if got["usr/bin/sudo"].Mode != 0o755 || got["usr/bin/wall"].Mode != 0o755 {
		t.Errorf("setuid/setgid not stripped: %o %o", got["usr/bin/sudo"].Mode, got["usr/bin/wall"].Mode)
	}
	if got["hard-ok"].Linkname != "etc/passwd" {
		t.Errorf("hard link %q", got["hard-ok"].Linkname)
	}
	if st.Kept != 7 || st.Dropped != 11 || st.Stripped != 2 {
		t.Errorf("stats %+v", st)
	}
	if _, err := FilterRootfs(strings.NewReader("not a tar stream at all, just text that is long enough to be a header block maybe"), io.Discard); err == nil {
		t.Error("garbage accepted")
	}
}
