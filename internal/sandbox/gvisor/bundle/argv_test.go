package bundle

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

const flags = "runsc --root=/st/runsc --network=sandbox --host-uds=none --host-fifo=none --overlay2=root:dir=/st/sbx-1/overlay"

func TestRunscArgv(t *testing.T) {
	r := Runsc{Root: "/st/runsc", Network: "sandbox", Overlay: "dir=/st/sbx-1/overlay"}
	tests := []struct {
		got  []string
		want string
	}{
		{r.Version(), "runsc --version"},
		{r.Run("/st/sbx-1", "sbx-1"), flags + " run --detach --bundle=/st/sbx-1 sbx-1"},
		{r.ExecProcess("sbx-1", "/st/sbx-1/proc/1.json", "/st/sbx-1/proc/1.pid"), flags + " exec --process=/st/sbx-1/proc/1.json --internal-pid-file=/st/sbx-1/proc/1.pid sbx-1"},
		{r.ExecAs("sbx-1", "1000:1000", "find", ".", "-type", "f"), flags + " exec --cwd=/workspace --user=1000:1000 sbx-1 find . -type f"},
		{r.Pause("sbx-1"), flags + " pause sbx-1"},
		{r.Resume("sbx-1"), flags + " resume sbx-1"},
		{r.Stats("sbx-1"), flags + " events --stats sbx-1"},
		{r.PS("sbx-1"), flags + " ps --format=json sbx-1"},
		{r.KillAll("sbx-1", "KILL"), flags + " kill --all sbx-1 KILL"},
		{r.KillPid("sbx-1", 7, "SIGTERM"), flags + " kill --pid=7 sbx-1 SIGTERM"},
		{r.Delete("sbx-1"), flags + " delete --force sbx-1"},
		{r.State("sbx-1"), flags + " state sbx-1"},
		{r.List(), flags + " list --quiet"},
		{Runsc{Root: "/r"}.Pause("x"), "runsc --root=/r --network=none --host-uds=none --host-fifo=none --overlay2=root:self pause x"},
	}
	for _, tt := range tests {
		if got := strings.Join(tt.got, " "); got != tt.want {
			t.Errorf("got  %s\nwant %s", got, tt.want)
		}
	}
}

func TestDockerAndHelperArgv(t *testing.T) {
	d := Docker{}
	tests := []struct {
		got  []string
		want string
	}{
		{d.Version(), "docker --version"},
		{d.Pull("debian:bookworm"), "docker pull -- debian:bookworm"},
		{d.ImageID("debian:bookworm"), "docker image inspect --format={{.Id}} -- debian:bookworm"},
		{d.Create("debian:bookworm"), "docker create --pull=never --network=none -- debian:bookworm /nonexistent"},
		{d.Export("c1"), "docker export -- c1"},
		{d.Remove("c1"), "docker rm --force -- c1"},
		{MkdirArgv("/a", "/b"), "mkdir -p -m 0700 -- /a /b"},
		{MkdirModeArgv(0o755, "/a"), "mkdir -p -m 755 -- /a"},
		{CreatePrivateArgv("/a/f"), "install -m 0600 /dev/null /a/f"},
		{WriteFileArgv("/a/f"), "dd of=/a/f bs=1M status=none"},
		{ChmodArgv(0o100755, "/w/x"), "chmod 755 -- /w/x"},
		{ChownArgv(User{UID: 1000, GID: 1001}, "/w"), "chown 1000:1001 -- /w"},
		{MoveArgv("/a.part.1", "/a"), "mv -f -T -- /a.part.1 /a"},
		{RemoveAllArgv("/st/sbx-1"), "rm -rf --one-file-system -- /st/sbx-1"},
		{TestDirArgv("/st/images/a"), "test -d /st/images/a"},
		{ExtractArgv("/st/images/a.part.1"), "tar -x --numeric-owner -C /st/images/a.part.1 -f -"},
		{DiskUsageArgv("/st/sbx-1"), "du -s -B1 -x -- /st/sbx-1"},
		{ToolVersionArgv("ip"), "ip -V"},
		{ToolVersionArgv("nft"), "nft --version"},
		{NetnsListArgv, "ip netns list"},
		{LinkListArgv, "ip -o link show"},
		{AddrListArgv, "ip -o -4 addr show"},
		{NFTTablesArgv, "nft list tables"},
		{IPTablesArgv, "iptables -w -S"},
		{IPTablesNatArgv, "iptables -w -t nat -S"},
		{IP6TablesArgv, "ip6tables -w -S"},
		{NFTScriptArgv, "nft -f -"},
	}
	for _, tt := range tests {
		if got := strings.Join(tt.got, " "); got != tt.want {
			t.Errorf("got  %s\nwant %s", got, tt.want)
		}
	}
}

func TestWSLArgv(t *testing.T) {
	h := WSLHost{}
	tests := []struct {
		got  []string
		want string
	}{
		{h.Wrap([]string{"runsc", "--version"}), "wsl.exe -d mycel-sbx -u root --exec runsc --version"},
		{h.WSLStatusArgv(), "wsl.exe --status"},
		{h.WSLListArgv(), "wsl.exe --list --quiet"},
		{h.DefaultRouteArgv(), "wsl.exe -d mycel-sbx -u root --exec ip route show default"},
		{WSLHost{Exe: `C:\Windows\System32\wsl.exe`, Distro: "other"}.Wrap([]string{"ip", "-V"}), `C:\Windows\System32\wsl.exe -d other -u root --exec ip -V`},
		{DetachArgv("/st/sbx-1/runsc-run.log", []string{"runsc", "run"}), "sh -s -- /st/sbx-1/runsc-run.log runsc run"},
	}
	for _, tt := range tests {
		if got := strings.Join(tt.got, " "); got != tt.want {
			t.Errorf("got  %s\nwant %s", got, tt.want)
		}
	}
	if got := (LocalHost{}).Wrap([]string{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("LocalHost.Wrap = %v", got)
	}
	if DetachScript != "log=$1; shift; exec setsid -w \"$@\" </dev/null >/dev/null 2>>\"$log\"\n" {
		t.Errorf("detach script %q", DetachScript)
	}
}

func TestHostCheck(t *testing.T) {
	for _, bad := range []string{`a"b`, "a\nb", "a\rb", "a\tb", "a\x00b", "a\x1bb", "a\x7fb"} {
		if err := (WSLHost{}).Check([]string{"ok", bad}); !errors.Is(err, ErrWSLArg) {
			t.Errorf("WSL accepted %q: %v", bad, err)
		}
	}
	if err := (WSLHost{}).Check([]string{"/var/lib/mycel/sbx/sbx-1", "{ type filter hook input priority 0 ; }", "--format={{.Id}}"}); err != nil {
		t.Errorf("WSL refused plain args: %v", err)
	}
	if err := (LocalHost{}).Check([]string{"a\x00"}); err == nil {
		t.Error("local accepted NUL")
	}
	if err := (LocalHost{}).Check([]string{`a"b`, "a\nb"}); err != nil {
		t.Errorf("local refused %v", err)
	}
}

func testNet(t *testing.T, fw Firewall, proxy string) Net {
	t.Helper()
	n, err := NewNet("sbx:01ABC", DefaultPool, 3)
	if err != nil {
		t.Fatal(err)
	}
	n.Firewall = fw
	n.Proxy = netip.MustParseAddrPort(proxy)
	n.DNS = n.Proxy.Addr()
	return n
}

func TestNewNet(t *testing.T) {
	n := testNet(t, FirewallNFT, "10.237.0.13:3128")
	s := ShortID("sbx:01ABC")
	if len(s) != 10 {
		t.Fatalf("short id %q", s)
	}
	if n.HostAddr.String() != "10.237.0.13" || n.PeerAddr.String() != "10.237.0.14" {
		t.Errorf("addresses %s %s", n.HostAddr, n.PeerAddr)
	}
	if n.Namespace != "mycel-"+s || n.HostIf != "myh"+s || n.PeerIf != "mys"+s {
		t.Errorf("names %+v", n)
	}
	if len(n.HostIf) > 15 || len(n.PeerIf) > 15 {
		t.Error("interface name longer than IFNAMSIZ-1")
	}
	if n.NetnsPath() != "/var/run/netns/mycel-"+s {
		t.Errorf("netns path %s", n.NetnsPath())
	}
	if _, err := NewNet("x", DefaultPool, 1<<14); err == nil {
		t.Error("slot out of range accepted")
	}
	if _, err := NewNet("x", netip.MustParsePrefix("fd00::/64"), 0); err == nil {
		t.Error("IPv6 pool accepted")
	}
}

func joinAll(cmds [][]string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func TestNetArgvNFTLocal(t *testing.T) {
	n := testNet(t, FirewallNFT, "10.237.0.13:3128")
	s := n.Short
	ns, h, p := "mycel-"+s, "myh"+s, "mys"+s
	tbl := "mycel_" + s
	want := []string{
		"ip netns add " + ns,
		"ip link add " + h + " type veth peer name " + p,
		"ip link set " + p + " netns " + ns,
		"ip addr add 10.237.0.13/30 dev " + h,
		"ip link set " + h + " up",
		"ip -n " + ns + " addr add 10.237.0.14/30 dev " + p,
		"ip -n " + ns + " link set " + p + " up",
		"ip -n " + ns + " link set lo up",
		"ip -n " + ns + " route add default via 10.237.0.13",
		"nft add table inet " + tbl,
		"nft add chain inet " + tbl + " input { type filter hook input priority 0 ; policy accept ; }",
		"nft add chain inet " + tbl + " forward { type filter hook forward priority 0 ; policy accept ; }",
		"nft add rule inet " + tbl + " input iifname " + h + " ip saddr 10.237.0.14 ip daddr 10.237.0.13 tcp dport 3128 accept",
		"nft add rule inet " + tbl + " input iifname " + h + " ip saddr 10.237.0.14 ip daddr 10.237.0.13 udp dport 53 accept",
		"nft add rule inet " + tbl + " input iifname " + h + " ip saddr 10.237.0.14 ip daddr 10.237.0.13 tcp dport 53 accept",
		"nft add rule inet " + tbl + " input iifname " + h + " drop",
		"nft add rule inet " + tbl + " forward iifname " + h + " drop",
		"nft add rule inet " + tbl + " forward oifname " + h + " drop",
	}
	if got := joinAll(n.SetupArgv()); !reflect.DeepEqual(got, want) {
		t.Errorf("setup:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantDown := []string{
		"nft delete table inet " + tbl,
		"ip link del " + h,
		"ip netns del " + ns,
	}
	if got := joinAll(n.TeardownArgv()); !reflect.DeepEqual(got, wantDown) {
		t.Errorf("teardown: %v", got)
	}
}

func TestNetArgvNFTForwarded(t *testing.T) {
	n := testNet(t, FirewallNFT, "172.22.16.1:41000")
	s := n.Short
	h, tbl := "myh"+s, "mycel_"+s
	got := joinAll(n.SetupArgv())
	want := []string{
		"sysctl -w net.ipv4.ip_forward=1",
		"nft add table inet " + tbl,
		"nft add chain inet " + tbl + " input { type filter hook input priority 0 ; policy accept ; }",
		"nft add chain inet " + tbl + " forward { type filter hook forward priority 0 ; policy accept ; }",
		"nft add rule inet " + tbl + " forward iifname " + h + " ip saddr 10.237.0.14 ip daddr 172.22.16.1 tcp dport 41000 accept",
		"nft add rule inet " + tbl + " forward iifname " + h + " ip saddr 10.237.0.14 ip daddr 172.22.16.1 udp dport 53 accept",
		"nft add rule inet " + tbl + " forward iifname " + h + " ip saddr 10.237.0.14 ip daddr 172.22.16.1 tcp dport 53 accept",
		"nft add rule inet " + tbl + " input iifname " + h + " drop",
		"nft add rule inet " + tbl + " forward oifname " + h + " ct state established,related accept",
		"nft add rule inet " + tbl + " forward iifname " + h + " drop",
		"nft add rule inet " + tbl + " forward oifname " + h + " drop",
		"nft add chain inet " + tbl + " postrouting { type nat hook postrouting priority 100 ; policy accept ; }",
		"nft add rule inet " + tbl + " postrouting ip saddr 10.237.0.14 oifname != " + h + " masquerade",
	}
	if !reflect.DeepEqual(got[9:], want) {
		t.Errorf("setup:\n%s\nwant:\n%s", strings.Join(got[9:], "\n"), strings.Join(want, "\n"))
	}
}

func TestNetArgvIPTables(t *testing.T) {
	n := testNet(t, FirewallIPTables, "10.237.0.13:3128")
	n.DNS = netip.Addr{}
	s := n.Short
	h, c := "myh"+s, "MYCEL-"+s
	got := joinAll(n.SetupArgv())[9:]
	want := []string{
		"iptables -w -N " + c,
		"iptables -w -A " + c + " -s 10.237.0.14 -d 10.237.0.13 -p tcp --dport 3128 -j ACCEPT",
		"iptables -w -A " + c + " -j DROP",
		"iptables -w -I INPUT -i " + h + " -j " + c,
		"iptables -w -I FORWARD -i " + h + " -j " + c,
		"iptables -w -I FORWARD -o " + h + " -j " + c,
		"ip6tables -w -I INPUT -i " + h + " -j DROP",
		"ip6tables -w -I FORWARD -i " + h + " -j DROP",
		"ip6tables -w -I FORWARD -o " + h + " -j DROP",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("setup:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	down := joinAll(n.TeardownArgv())
	wantDown := []string{
		"iptables -w -t nat -D POSTROUTING -s 10.237.0.14 ! -o " + h + " -j MASQUERADE",
		"ip6tables -w -D FORWARD -o " + h + " -j DROP",
		"ip6tables -w -D FORWARD -i " + h + " -j DROP",
		"ip6tables -w -D INPUT -i " + h + " -j DROP",
		"iptables -w -D FORWARD -o " + h + " -j " + c,
		"iptables -w -D FORWARD -i " + h + " -j " + c,
		"iptables -w -D INPUT -i " + h + " -j " + c,
		"iptables -w -F " + c,
		"iptables -w -X " + c,
		"ip link del " + h,
		"ip netns del mycel-" + s,
	}
	if !reflect.DeepEqual(down, wantDown) {
		t.Errorf("teardown:\n%s", strings.Join(down, "\n"))
	}
}

func TestNetArgvIPTablesForwarded(t *testing.T) {
	n := testNet(t, FirewallIPTables, "172.22.16.1:41000")
	s := n.Short
	h, c := "myh"+s, "MYCEL-"+s
	got := joinAll(n.SetupArgv())
	if got[9] != "sysctl -w net.ipv4.ip_forward=1" {
		t.Errorf("no ip_forward: %s", got[9])
	}
	for _, w := range []string{
		"iptables -w -A " + c + " -s 10.237.0.14 -d 172.22.16.1 -p tcp --dport 41000 -j ACCEPT",
		"iptables -w -A " + c + " -s 10.237.0.14 -d 172.22.16.1 -p udp --dport 53 -j ACCEPT",
		"iptables -w -A " + c + " -o " + h + " -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"iptables -w -t nat -A POSTROUTING -s 10.237.0.14 ! -o " + h + " -j MASQUERADE",
	} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %s", w)
		}
	}
	if got[len(got)-1] != "iptables -w -t nat -A POSTROUTING -s 10.237.0.14 ! -o "+h+" -j MASQUERADE" {
		t.Errorf("last rule %s", got[len(got)-1])
	}
}

func TestSlotAllocator(t *testing.T) {
	a := NewSlotAllocator(netip.MustParsePrefix("10.0.0.0/29")) // two slots
	s1, err := a.Get("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a.Get("b", nil)
	if err != nil || s2 == s1 {
		t.Fatalf("second slot %d %v", s2, err)
	}
	if _, err := a.Get("c", nil); err == nil {
		t.Fatal("pool not exhausted")
	}
	a.Release("a")
	if s, err := a.Get("c", nil); err != nil || s != s1 {
		t.Fatalf("reuse %d %v", s, err)
	}
}

func TestParseDefaultGateway(t *testing.T) {
	a, err := ParseDefaultGateway([]byte("default via 172.22.16.1 dev eth0 proto kernel\n"))
	if err != nil || a.String() != "172.22.16.1" {
		t.Errorf("%v %v", a, err)
	}
	if _, err := ParseDefaultGateway([]byte("10.0.0.0/8 dev eth0\n")); err == nil {
		t.Error("no default route accepted")
	}
}
