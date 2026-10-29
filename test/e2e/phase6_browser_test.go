//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	fh "github.com/richard-sen27/mycelium/test/fakeharness"
)

// The opt-in browser smoke test (MYCEL_TEST_BROWSER=1): a headless Chrome
// loads the hub UI, signs in with the admin API key, sees the rail, opens
// Approvals and approves a pending grant on its card; then it opens the
// node UI through `mycel ui --print` and sees the stale banner once the hub
// is stopped. It needs a UI build in the binary (`make web`) and Chrome or
// Chromium; MYCEL_TEST_BROWSER_DIR is where the throwaway profile goes
// (default: the system temp dir). chromedp is used by this file only (D025).

const envTestBrowser = "MYCEL_TEST_BROWSER"

// chromePath finds Chrome or Chromium, "" when there is none.
func chromePath() string {
	if p := os.Getenv("MYCEL_TEST_CHROME"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// waitText polls the page until its text contains want.
func waitText(ctx context.Context, t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	var text string
	deadline := time.Now().Add(timeout)
	for {
		s, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.body ? document.body.innerText : ""`))
		if err == nil {
			text = s
			if strings.Contains(text, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page never showed %q within %s; it shows:\n%s", want, timeout, text)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestUIBrowserSmoke(t *testing.T) {
	if os.Getenv(envTestBrowser) != "1" {
		t.Skip("set MYCEL_TEST_BROWSER=1 to drive a headless Chrome through the UI")
	}
	chrome := chromePath()
	if chrome == "" {
		t.Skip("no Chrome or Chromium found (set MYCEL_TEST_CHROME)")
	}
	h := startHub(t)
	up := newFakeOllama(t)
	alpha := joinNode(t, h, "alpha", "ollama,sandbox", "", fakeOK, "MYCEL_OLLAMA_UPSTREAM="+up.URL)
	waitConnected(t, alpha)
	// No auto rule: the admin's request waits for a decision.
	mustRun(t, alpha, "llm", []fh.Step{{Tool: "whoami", Expect: "agent:llm@alpha"}})
	var req struct {
		Grant jsonGrant `json:"grant"`
	}
	mustJSON(t, h.MustCLI("", "grants", "request", "--kind", "ollama", "--subject", "agent:llm@alpha", "--duration", "10m",
		"--reason", "browser smoke", "--json"), &req)
	g := req.Grant
	if g.State != grantPending {
		t.Fatalf("grant %+v; want pending", g)
	}

	profile, err := os.MkdirTemp(os.Getenv("MYCEL_TEST_BROWSER_DIR"), "mycel-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) }) //nolint:gosec // our own temp dir
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(profile),
		chromedp.WindowSize(1280, 900),
		// The hub's certificate chains to its own CA, which this throwaway
		// profile does not trust.
		chromedp.IgnoreCertErrors,
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("no-service-autorun", true),
	)
	actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	bctx, cancelBrowser := chromedp.NewContext(actx, chromedp.WithLogf(t.Logf))
	defer cancelBrowser()
	ctx, cancel := context.WithTimeout(bctx, 3*time.Minute)
	defer cancel()
	rail := chromedp.CSS(`nav[aria-label="Views"]`)
	// The page's console, shown when the test fails.
	var (
		consoleMu sync.Mutex
		console   []string
	)
	if err := chromedp.Do(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatalf("starting %s: %v", chrome, err)
	}
	cctx, stopConsole := context.WithCancel(ctx)
	consoleDone := make(chan struct{})
	go func() {
		defer close(consoleDone)
		for m, err := range chromedp.Console(cctx) {
			if err != nil {
				return
			}
			consoleMu.Lock()
			console = append(console, fmt.Sprintf("%s: %s %s:%d", m.Type, m.Text, m.URL, m.Line))
			consoleMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		stopConsole()
		<-consoleDone
		if t.Failed() {
			consoleMu.Lock()
			t.Logf("==== browser console ====\n%s", strings.Join(console, "\n"))
			consoleMu.Unlock()
		}
	})
	// step runs browser actions with a 30 s limit and shows the page on failure.
	step := func(what string, actions ...chromedp.Action[chromedp.Void]) {
		t.Helper()
		sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
		defer scancel()
		if err := chromedp.Do(sctx, actions...); err != nil {
			loc, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`location.href`))
			text, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`document.body ? document.body.innerText : ""`))
			t.Fatalf("%s: %v\nat %s the page shows:\n%s", what, err, loc, text)
		}
	}

	// The hub UI: sign in with the API key, see the rail.
	start := time.Now()
	step("hub UI: the sign-in screen", chromedp.Navigate(h.HubURL+"/"), chromedp.WaitVisible(chromedp.CSS(`input[type="password"]`)))
	// Enter submits the form. (A mouse click within the first ~0.5 s can
	// miss: the sign-in frame assembles with a clip-path animation, and
	// clipped-out areas are not hit-testable until it ends.)
	step("hub UI: sign in with the API key",
		chromedp.SendKeys(chromedp.CSS(`input[type="password"]`), h.AdminKey+kb.Enter),
		chromedp.WaitVisible(rail),
	)
	waitText(ctx, t, "hub ui", 10*time.Second)
	t.Logf("hub UI: signed in, rail visible after %s", time.Since(start).Round(time.Millisecond))

	// Approvals: the card of the pending grant, approved with its button.
	card := `#grant-` + g.ID
	step("hub UI: open Approvals",
		chromedp.Click(chromedp.CSS(`nav[aria-label="Views"] a[href="/approvals"]`)),
		chromedp.WaitVisible(chromedp.CSS(card)),
	)
	approve := `(() => {
		const b = [...document.querySelectorAll(` + strconv.Quote(card+" button") + `)].find((x) => x.textContent.trim().startsWith('Approve'));
		if (!b) return false;
		b.click();
		return true;
	})()`
	if clicked, err := chromedp.Run(ctx, chromedp.Evaluate[bool](approve)); err != nil || !clicked {
		t.Fatalf("no Approve button on the card of %s (err %v)", g.ID, err)
	}
	waitGrant(t, h, g.ID, 15*time.Second, "active", func(x jsonGrant) bool { return x.State == grantActive })
	waitText(ctx, t, "Approved "+g.ID, 10*time.Second)
	t.Logf("hub UI: approved %s on its card", g.ID)

	// The node UI through `mycel ui --print`; stale once the hub is gone.
	out, errOut, code := nodeCLI(t, alpha, "ui", "--print")
	u := strings.TrimSpace(out)
	if code != 0 || !strings.Contains(u, "#local=") {
		t.Fatalf("mycel ui --print: exit %d\n%s\n%s", code, out, errOut)
	}
	step("node UI: the rail", chromedp.Navigate(u), chromedp.WaitVisible(rail))
	waitText(ctx, t, "node ui", 10*time.Second)
	if hash, err := chromedp.Run(ctx, chromedp.Evaluate[string](`location.hash`)); err != nil || strings.Contains(hash, "local=") {
		t.Errorf("the node UI kept the token in the URL fragment: %q (err %v)", hash, err)
	}
	h.Proc.stop()
	stopped := time.Now()
	waitText(ctx, t, "Hub unreachable", 60*time.Second)
	t.Logf("node UI: stale banner %s after the hub stopped", time.Since(stopped).Round(time.Millisecond))
}
