//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Phase 1 "Done when": a hub and two nodes join with tokens; a reused or
// expired token is refused; a revoked node is disconnected within 5 seconds;
// `mycel nodes ls` shows both nodes with inventory.

const exitRevoked = 3

func TestJoinTwoNodes(t *testing.T) {
	h := startHub(t)
	alpha := joinNode(t, h, "alpha", "docker,ollama", "")
	beta := joinNode(t, h, "beta", "gpu", "")

	wantCaps := map[string][]string{
		"alpha": {"docker", "ollama"},
		"beta":  {"gpu"},
	}
	var nodes map[string]jsonNode
	ok := waitFor(t, 15*time.Second, func() bool {
		nodes = listNodes(t, h, "")
		for _, name := range []string{"hub-node", "alpha", "beta"} {
			n, found := nodes[name]
			if !found || !n.Online || n.Inventory.OS == "" {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("hub-node, alpha and beta not all online with inventory within 15s: %+v", nodes)
	}
	for _, name := range []string{"hub-node", "alpha", "beta"} {
		inv := nodes[name].Inventory
		if inv.OS == "" || inv.Arch == "" || inv.Hostname == "" || inv.CPUCount <= 0 || inv.memBytes() <= 0 {
			t.Errorf("%s: incomplete inventory: %+v", name, inv)
		}
	}
	for name, want := range wantCaps {
		got := slices.Clone(nodes[name].CapsCeiling.Names)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: caps_ceiling = %v, want %v", name, got, want)
		}
	}

	out := h.MustCLI("", "nodes", "get", "alpha", "--json")
	var get struct {
		Node jsonNode `json:"node"`
	}
	mustJSON(t, out, &get)
	if get.Node.Name != "alpha" || get.Node.Inventory.Hostname == "" {
		t.Errorf("nodes get alpha: unexpected %s", out)
	}

	for _, n := range []*Node{alpha, beta} {
		if !waitFor(t, 10*time.Second, func() bool {
			st := n.Status(t)
			return st != nil && st["connected"] == true
		}) {
			t.Errorf("%s: status never reported connected: %v", n.Name, n.Status(t))
		}
	}

	human := h.MustCLI("", "nodes", "ls")
	for _, name := range []string{"hub-node", "alpha", "beta"} {
		if !strings.Contains(human, name) {
			t.Errorf("nodes ls output lacks %q:\n%s", name, human)
		}
	}
}

func TestTokenReuseRefused(t *testing.T) {
	h := startHub(t)
	gamma := joinNode(t, h, "gamma", "", "")

	fresh := filepath.Join(h.Root, "gamma-again")
	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+fresh), "join", gamma.Token, "--json")
	if code != 1 {
		t.Fatalf("reused token: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(strings.ToLower(errOut), "token") {
		t.Errorf("reused token: stderr does not mention the token: %q", errOut)
	}

	out = h.MustCLI("", "nodes", "ls", "--json")
	var resp struct {
		Nodes []jsonNode `json:"nodes"`
	}
	mustJSON(t, out, &resp)
	count := 0
	for _, n := range resp.Nodes {
		if n.Name == "gamma" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("hub lists %d nodes named gamma, want 1: %s", count, out)
	}
}

func TestTokenExpiredRefused(t *testing.T) {
	h := startHub(t)
	tok := createToken(t, h, "", "--name", "delta", "--ttl", "1s")
	time.Sleep(2 * time.Second) // the spec's scenario: let the token expire

	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+filepath.Join(h.Root, "delta")), "join", tok.Token, "--json")
	if code != 1 {
		t.Fatalf("expired token: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	var found bool
	for _, jt := range listTokens(t, h, "") {
		if jt.ID == tok.JoinToken.ID {
			found = true
			if jt.UsedAt != "" {
				t.Errorf("expired token has used_at %q, want empty", jt.UsedAt)
			}
		}
	}
	if !found {
		t.Errorf("token ls does not list token %s", tok.JoinToken.ID)
	}
	if _, ok := listNodes(t, h, "")["delta"]; ok {
		t.Errorf("node delta exists after an expired-token join")
	}
}

func TestTamperedTokenRefused(t *testing.T) {
	h := startHub(t)
	tok := createToken(t, h, "", "--name", "delta2")

	b := []byte(tok.Token)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	tampered := string(b)

	out, errOut, code := runCLI(t, childEnv("MYCEL_HOME="+filepath.Join(h.Root, "delta2")), "join", tampered, "--json")
	if code != 1 {
		t.Fatalf("tampered token: exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if _, ok := listNodes(t, h, "")["delta2"]; ok {
		t.Errorf("node delta2 exists after a tampered-token join")
	}
}

func TestRevokedNodeDisconnected(t *testing.T) {
	h := startHub(t)
	eps := joinNode(t, h, "epsilon", "", "")
	if !waitFor(t, 15*time.Second, func() bool { return listNodes(t, h, "")["epsilon"].Online }) {
		t.Fatal("epsilon never came online")
	}

	h.MustCLI("", "nodes", "rm", "epsilon")
	removed := time.Now()
	code, ok := eps.Wait(5 * time.Second)
	if !ok {
		t.Fatalf("epsilon daemon still running 5s after nodes rm")
	}
	t.Logf("epsilon daemon exited %s after nodes rm", time.Since(removed).Round(time.Millisecond))
	if code != exitRevoked {
		t.Errorf("epsilon daemon exit code %d, want %d", code, exitRevoked)
	}
	if _, err := os.Stat(filepath.Join(eps.Home, "data", "node", "node.key")); !os.IsNotExist(err) {
		t.Errorf("node.key still present after revocation (stat err: %v)", err)
	}
	if _, ok := listNodes(t, h, "")["epsilon"]; ok {
		t.Errorf("nodes ls still lists epsilon")
	}
	if strings.Contains(readFile(h.Proc.stderrPath), eps.Token) {
		t.Errorf("hub log contains the join token")
	}
}

func TestMemberRole(t *testing.T) {
	h := startHub(t)
	out := h.MustCLI("", "user", "add", "bob", "--role", "member", "--json")
	var bob struct {
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		APIKey string `json:"api_key"`
	}
	mustJSON(t, out, &bob)
	if bob.APIKey == "" || bob.User.ID == "" {
		t.Fatalf("user add: missing key or id: %s", out)
	}
	key := bob.APIKey

	bobTok := createToken(t, h, key, "--name", "bobbox")
	listNodes(t, h, key) // a member may list nodes

	_, errOut, code := h.CLIAs(key, "user", "add", "carol", "--role", "member")
	if code != 1 || !strings.Contains(strings.ToLower(errOut), "permission") {
		t.Errorf("member user add: exit %d, stderr %q; want exit 1 and a permission error", code, errOut)
	}
	_, errOut, code = h.CLIAs(key, "nodes", "rm", "hub-node")
	if code != 1 || !strings.Contains(strings.ToLower(errOut), "permission") {
		t.Errorf("member nodes rm hub-node: exit %d, stderr %q; want exit 1 and a permission error", code, errOut)
	}

	adminTok := createToken(t, h, "", "--name", "adminbox")
	bobToks := listTokens(t, h, key)
	if len(bobToks) != 1 || bobToks[0].ID != bobTok.JoinToken.ID {
		t.Errorf("bob's token ls = %+v, want only %s", bobToks, bobTok.JoinToken.ID)
	}
	ids := map[string]bool{}
	for _, jt := range listTokens(t, h, "") {
		ids[jt.ID] = true
	}
	if !ids[bobTok.JoinToken.ID] || !ids[adminTok.JoinToken.ID] {
		t.Errorf("admin token ls lacks bob's or admin's token: %v", ids)
	}

	box := joinWithToken(t, h, "bobbox", bobTok.Token)
	out = h.MustCLI("", "nodes", "get", "bobbox", "--json")
	var get struct {
		Node jsonNode `json:"node"`
	}
	mustJSON(t, out, &get)
	if get.Node.OwnerID != bob.User.ID {
		t.Errorf("bobbox owner_id = %q, want bob's id %q", get.Node.OwnerID, bob.User.ID)
	}

	h.MustCLI(key, "nodes", "rm", "bobbox")
	code, ok := box.Wait(5 * time.Second)
	if !ok || code != exitRevoked {
		t.Errorf("bobbox daemon after bob's nodes rm: exited=%v code=%d, want exit %d", ok, code, exitRevoked)
	}
}

func TestWhoamiAndLogin(t *testing.T) {
	h := startHub(t)
	out := h.MustCLI("", "whoami", "--json")
	var who struct {
		User struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
	}
	mustJSON(t, out, &who)
	if who.User.Role != "ROLE_ADMIN" {
		t.Errorf("admin whoami role = %q, want ROLE_ADMIN", who.User.Role)
	}

	out = h.MustCLI("", "user", "add", "bob", "--role", "member", "--json")
	var bob struct {
		APIKey string `json:"api_key"`
	}
	mustJSON(t, out, &bob)

	env := childEnv("MYCEL_HOME=" + filepath.Join(h.Root, "bob-laptop"))
	out, errOut, code := runCLI(t, env, "login", h.HubURL, "--api-key", bob.APIKey, "--ca-fingerprint", h.CAFingerprint, "--json")
	if code != 0 {
		t.Fatalf("login: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	out, errOut, code = runCLI(t, env, "whoami", "--json")
	if code != 0 {
		t.Fatalf("whoami after login: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	mustJSON(t, out, &who)
	if who.User.Name != "bob" || who.User.Role != "ROLE_MEMBER" {
		t.Errorf("whoami after login = %+v, want bob/ROLE_MEMBER", who.User)
	}
}

func TestHubRestartNodesReconnect(t *testing.T) {
	h := startHub(t)
	zeta := joinNode(t, h, "zeta", "", "")
	connected := func() bool {
		st := zeta.Status(t)
		return st != nil && st["connected"] == true
	}
	if !waitFor(t, 15*time.Second, connected) {
		t.Fatalf("zeta never connected: %v", zeta.Status(t))
	}

	h.Proc.kill()
	// Best effort: the node should notice the hub is gone. Reconnect timing
	// is not asserted here, so only log what was observed.
	var seen any
	if waitFor(t, 10*time.Second, func() bool {
		st := zeta.Status(t)
		if st != nil && st["connected"] != true {
			seen = st["state"]
			return true
		}
		return false
	}) {
		t.Logf("zeta state while hub is down: %v", seen)
	} else {
		t.Logf("zeta did not report a disconnect within 10s of the hub being killed")
	}

	h.Start()
	if !waitFor(t, 20*time.Second, func() bool {
		return listNodes(t, h, "")["zeta"].Online && connected()
	}) {
		t.Fatalf("zeta not online and connected within 20s of hub restart: hub=%+v status=%v",
			listNodes(t, h, "")["zeta"], zeta.Status(t))
	}
}
