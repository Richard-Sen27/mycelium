package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// fakeRunner records commands and answers from a table keyed by the full
// command line; unknown commands succeed with no output.
type fakeRunner struct {
	calls   []string
	answers map[string]fakeAnswer
}

type fakeAnswer struct {
	out string
	err error
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if a, ok := f.answers[line]; ok {
		return a.out, a.err
	}
	return "", nil
}

var errExit = errors.New("exit status 1")

func testEnv(t *testing.T, run Runner) env {
	t.Helper()
	root := t.TempDir()
	return env{
		home:       "/Users/ann/mycel-home",
		logDir:     filepath.Join(root, "logs"),
		userHome:   root,
		configHome: filepath.Join(root, ".config"),
		uid:        501,
		username:   `PC\ann`,
		systemRoot: `C:\Windows`,
		path:       "/opt/homebrew/bin:/usr/bin:/bin",
		exe:        "/Users/ann/.local/bin/mycel",
		tempDirs:   []string{"/private/var/folders", "/tmp"},
		run:        run,
		sleep:      func(time.Duration) {},
		findPID:    func(string) int { return 0 },
		killPID:    func(int) error { return nil },
	}
}

func TestRenderPlist(t *testing.T) {
	e := env{home: "/Users/ann/mycel home", logDir: "/Users/ann/mycel home/logs", path: "/usr/bin:/bin", exe: "/Users/ann/.local/bin/mycel"}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by ` + "`mycel service install`" + ` (role: node). Remove with ` + "`mycel service uninstall`" + `. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.mycel.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/ann/.local/bin/mycel</string>
		<string>daemon</string>
		<string>--log-file</string>
		<string>/Users/ann/mycel home/logs/daemon.log</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>MYCEL_HOME</key>
		<string>/Users/ann/mycel home</string>
		<key>PATH</key>
		<string>/usr/bin:/bin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>/Users/ann/mycel home/logs/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/ann/mycel home/logs/daemon.log</string>
</dict>
</plist>
`
	if got := renderPlist(e, "node"); got != want {
		t.Errorf("plist:\n%s\nwant:\n%s", got, want)
	}
	// XML special characters are escaped; no MYCEL_HOME means no entry.
	e = env{logDir: "/l", exe: "/a&b/<mycel>"}
	got := renderPlist(e, "hub")
	if !strings.Contains(got, "<string>/a&amp;b/&lt;mycel&gt;</string>") || strings.Contains(got, "EnvironmentVariables") {
		t.Errorf("escaping or empty env wrong:\n%s", got)
	}
}

func TestLaunchdCommands(t *testing.T) {
	r := &fakeRunner{answers: map[string]fakeAnswer{
		"launchctl print gui/501/io.mycel.daemon": {err: errExit},
	}}
	e := testEnv(t, r)
	m, err := newManager("darwin", e)
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(e.userHome, "Library", "LaunchAgents", "io.mycel.daemon.plist")

	if err := m.Install("node"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "launchctl print gui/501/io.mycel.daemon", "launchctl bootstrap gui/501 "+plist)
	b, err := os.ReadFile(plist)
	if err != nil || string(b) != renderPlist(e, "node") {
		t.Fatalf("plist on disk: %v\n%s", err, b)
	}
	if _, err := os.Stat(e.logDir); err != nil {
		t.Errorf("log dir not created: %v", err)
	}

	// Loaded now: status parses launchctl print; reinstall boots out first.
	r.answers["launchctl print gui/501/io.mycel.daemon"] = fakeAnswer{out: "gui/501/io.mycel.daemon = {\n\tactive count = 1\n\tstate = running\n\n\tprogram = /Users/ann/.local/bin/mycel\n\tpid = 4242\n}\n"}
	st, err := m.Status()
	if err != nil || st != (Status{Installed: true, Running: true, PID: 4242, Path: plist}) {
		t.Errorf("status = %+v, %v", st, err)
	}
	r.calls = nil
	if err := m.Install("hub"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "launchctl print gui/501/io.mycel.daemon", "launchctl bootout gui/501/io.mycel.daemon", "launchctl bootstrap gui/501 "+plist)

	r.calls = nil
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "launchctl print gui/501/io.mycel.daemon", "launchctl kickstart gui/501/io.mycel.daemon",
		"launchctl print gui/501/io.mycel.daemon", "launchctl bootout gui/501/io.mycel.daemon")

	r.calls = nil
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "launchctl print gui/501/io.mycel.daemon", "launchctl bootout gui/501/io.mycel.daemon")
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
	r.answers["launchctl print gui/501/io.mycel.daemon"] = fakeAnswer{err: errExit}
	if err := m.Uninstall(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("second uninstall: %v, want ErrNotInstalled", err)
	}
	if err := m.Start(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("start when not installed: %v", err)
	}
	st, _ = m.Status()
	if st.Installed || st.Running {
		t.Errorf("status after uninstall: %+v", st)
	}
}

func TestLaunchdBootstrapRetries(t *testing.T) {
	r := &fakeRunner{answers: map[string]fakeAnswer{"launchctl print gui/501/io.mycel.daemon": {err: errExit}}}
	e := testEnv(t, r)
	plist := filepath.Join(e.userHome, "Library", "LaunchAgents", "io.mycel.daemon.plist")
	r.answers["launchctl bootstrap gui/501 "+plist] = fakeAnswer{err: errors.New("Bootstrap failed: 5: Input/output error")}
	m, _ := newManager("darwin", e)
	if err := m.Install(""); err == nil || !strings.Contains(err.Error(), "Bootstrap failed") {
		t.Fatalf("install: %v", err)
	}
	if n := strings.Count(strings.Join(r.calls, "\n"), "bootstrap"); n != 5 {
		t.Errorf("bootstrap attempts = %d, want 5", n)
	}
}

func TestRenderUnit(t *testing.T) {
	e := env{home: "/home/ann/my home", logDir: "/home/ann/my home/logs", path: "/usr/bin:/bin", exe: "/home/ann/.local/bin/mycel"}
	want := "# Written by `mycel service install` (role: node). Remove with `mycel service uninstall`.\n" +
		"[Unit]\nDescription=Mycelium daemon (mycel, role node)\nStartLimitIntervalSec=0\n\n" +
		"[Service]\nType=simple\n" +
		"ExecStart=\"/home/ann/.local/bin/mycel\" daemon --log-file \"/home/ann/my home/logs/daemon.log\"\n" +
		"Environment=\"MYCEL_HOME=/home/ann/my home\"\n" +
		"Environment=\"PATH=/usr/bin:/bin\"\n" +
		"Restart=on-failure\nRestartSec=5\n" +
		"# exit 3: this node was revoked; restarting cannot help\nRestartPreventExitStatus=3\n" +
		"# append: needs systemd 240 or newer\n" +
		"StandardOutput=append:/home/ann/my home/logs/daemon.log\nStandardError=append:/home/ann/my home/logs/daemon.log\n\n" +
		"[Install]\nWantedBy=default.target\n"
	if got := renderUnit(e, "node"); got != want {
		t.Errorf("unit:\n%s\nwant:\n%s", got, want)
	}
	// Without MYCEL_HOME the XDG directories are recorded.
	e.home = ""
	e.xdg = [][2]string{{"XDG_DATA_HOME", "/data/ann"}}
	if got := renderUnit(e, "node"); !strings.Contains(got, "Environment=\"XDG_DATA_HOME=/data/ann\"\nEnvironment=\"PATH=") {
		t.Errorf("unit without MYCEL_HOME:\n%s", got)
	}
	if got := systemdQuote(`/a b/$x%i"q\`); got != `"/a b/$$x%%i\"q\\"` {
		t.Errorf("systemdQuote = %s", got)
	}
}

func TestSystemdCommands(t *testing.T) {
	r := &fakeRunner{answers: map[string]fakeAnswer{}}
	e := testEnv(t, r)
	m, _ := newManager("linux", e)
	unit := filepath.Join(e.configHome, "systemd", "user", "mycel.service")

	if err := m.Install("node"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "systemctl --user daemon-reload", "systemctl --user enable --now mycel.service")
	if b, err := os.ReadFile(unit); err != nil || string(b) != renderUnit(e, "node") {
		t.Fatalf("unit on disk: %v\n%s", err, b)
	}
	if n, ok := m.(Noter); !ok || !strings.Contains(strings.Join(n.Notes(), "\n"), "loginctl enable-linger $USER") {
		t.Error("no linger hint")
	}

	r.calls = nil
	if err := m.Install("node"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "systemctl --user daemon-reload", "systemctl --user enable --now mycel.service", "systemctl --user restart mycel.service")

	r.answers["systemctl --user is-active mycel.service"] = fakeAnswer{out: "active\n"}
	r.answers["systemctl --user show mycel.service --property=MainPID --value"] = fakeAnswer{out: "777\n"}
	if st, err := m.Status(); err != nil || st != (Status{Installed: true, Running: true, PID: 777, Path: unit}) {
		t.Errorf("status = %+v, %v", st, err)
	}
	r.answers["systemctl --user is-active mycel.service"] = fakeAnswer{out: "inactive\n", err: errExit}
	if st, _ := m.Status(); st.Running || st.PID != 0 || !st.Installed {
		t.Errorf("inactive status = %+v", st)
	}

	r.calls = nil
	_ = m.Start()
	_ = m.Stop()
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "systemctl --user start mycel.service", "systemctl --user stop mycel.service",
		"systemctl --user disable --now mycel.service", "systemctl --user daemon-reload")
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there: %v", err)
	}
	if err := m.Stop(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("stop when not installed: %v", err)
	}
}

func TestRenderTaskXML(t *testing.T) {
	e := env{username: `PC\ann`, systemRoot: `C:\Windows`, logDir: `C:\Users\ann\AppData\Local\mycel\logs`,
		exe: `C:\Users\ann\AppData\Local\mycel\bin\mycel.exe`}
	m := &schtasks{e}
	if got := m.launcherPath(); got != `C:\Users\ann\AppData\Local\mycel\mycel-daemon.cmd` {
		t.Errorf("launcher path = %s", got)
	}
	got := renderTaskXML(e, "node", m.launcherPath())
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		"<LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>PC\\ann</UserId>\n    </LogonTrigger>",
		"<LogonType>InteractiveToken</LogonType>\n      <RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<RestartOnFailure>\n      <Interval>PT1M</Interval>\n      <Count>999</Count>\n    </RestartOnFailure>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		`<Command>C:\Windows\System32\conhost.exe</Command>`,
		`<Arguments>--headless C:\Windows\System32\cmd.exe /d /s /c &#34;C:\Users\ann\AppData\Local\mycel\mycel-daemon.cmd&#34;</Arguments>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("task XML lacks %q:\n%s", want, got)
		}
	}
	loop := ":run\r\n" +
		`C:\Users\ann\AppData\Local\mycel\bin\mycel.exe daemon --log-file C:\Users\ann\AppData\Local\mycel\logs\daemon.log` + "\r\n" +
		"set \"rc=%ERRORLEVEL%\"\r\n" +
		"rem 0: stopped; 3: this node was revoked, restarting cannot help.\r\n" +
		"if \"%rc%\"==\"0\" exit /b 0\r\n" +
		"if \"%rc%\"==\"3\" exit /b 3\r\n" +
		"rem wait 10 s (timeout.exe needs a console input)\r\n" +
		"ping -n 11 127.0.0.1 >nul\r\n" +
		"goto run\r\n"
	head := "@echo off\r\nrem Written by `mycel service install`. Remove with `mycel service uninstall`.\r\nsetlocal\r\n"
	if got := renderLauncher(e); got != head+loop {
		t.Errorf("launcher:\n%q\nwant\n%q", got, head+loop)
	}
	// With MYCEL_HOME (and a % and spaces in the paths): the launcher sets
	// it, escaped for cmd.
	e.home = `C:\my 100% home`
	e.logDir = `C:\my 100% home\logs`
	m = &schtasks{e}
	got = renderTaskXML(e, "hub", m.launcherPath())
	if want := `<Arguments>--headless C:\Windows\System32\cmd.exe /d /s /c &#34;&#34;C:\my 100% home\mycel-daemon.cmd&#34;&#34;</Arguments>`; !strings.Contains(got, want) {
		t.Errorf("task XML lacks %s:\n%s", want, got)
	}
	launcher := renderLauncher(e)
	for _, want := range []string{
		"set \"MYCEL_HOME=C:\\my 100%% home\"\r\n:run\r\n",
		`C:\Users\ann\AppData\Local\mycel\bin\mycel.exe daemon --log-file "C:\my 100%% home\logs\daemon.log"` + "\r\n",
	} {
		if !strings.Contains(launcher, want) {
			t.Errorf("launcher lacks %q:\n%s", want, launcher)
		}
	}
	// UTF-16LE with a BOM.
	b := encodeUTF16("<a>é</a>")
	if b[0] != 0xFF || b[1] != 0xFE || len(b) != 2+2*8 {
		t.Errorf("encodeUTF16 = % x", b)
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
	}
	if string(utf16.Decode(u)) != "<a>é</a>" {
		t.Error("encodeUTF16 does not round-trip")
	}
}

func TestQuoteWinArg(t *testing.T) {
	for in, want := range map[string]string{
		`C:\bin\mycel.exe`:    `C:\bin\mycel.exe`,
		`C:\Program Files\m`:  `"C:\Program Files\m"`,
		"":                    `""`,
		`a "b"`:               `"a \"b\""`,
		`C:\dir with space\\`: `"C:\dir with space\\\\"`,
		`x\"y z`:              `"x\\\"y z"`,
	} {
		if got := quoteWinArg(in); got != want {
			t.Errorf("quoteWinArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSchtasksCommands(t *testing.T) {
	r := &fakeRunner{answers: map[string]fakeAnswer{
		"schtasks /Query /TN Mycel /FO CSV /NH": {err: errExit},
	}}
	e := testEnv(t, r)
	e.home = filepath.Join(e.userHome, "home")
	killed := 0
	pid := 0
	e.findPID = func(string) int { return pid }
	e.killPID = func(p int) error { killed = p; return nil }
	m, _ := newManager("windows", e)
	xmlFile := filepath.Join(e.logDir, "mycel-task.xml")

	if err := m.Install("node"); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "schtasks /Query /TN Mycel /FO CSV /NH",
		"schtasks /Create /TN Mycel /XML "+xmlFile+" /F",
		"schtasks /Run /TN Mycel")
	if _, err := os.Stat(xmlFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("task XML left behind: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(e.userHome, "mycel-daemon.cmd")); err != nil || !bytes.Contains(b, []byte("MYCEL_HOME="+e.home)) {
		t.Errorf("launcher: %v\n%s", err, b)
	}

	r.answers["schtasks /Query /TN Mycel /FO CSV /NH"] = fakeAnswer{out: `"\Mycel","N/A","Running"` + "\n"}
	pid = 9001
	if st, err := m.Status(); err != nil || st != (Status{Installed: true, Running: true, PID: 9001, Path: "Mycel"}) {
		t.Errorf("status = %+v, %v", st, err)
	}

	r.calls = nil
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if killed != 9001 {
		t.Errorf("stop did not end the leftover daemon (killed %d)", killed)
	}
	_ = m.Start()
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, "schtasks /Query /TN Mycel /FO CSV /NH", "schtasks /End /TN Mycel",
		"schtasks /Query /TN Mycel /FO CSV /NH", "schtasks /Run /TN Mycel",
		"schtasks /Query /TN Mycel /FO CSV /NH", "schtasks /End /TN Mycel", "schtasks /Delete /TN Mycel /F")
	if _, err := os.Stat(filepath.Join(e.userHome, "mycel-daemon.cmd")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("launcher left behind: %v", err)
	}
}

func TestInstallRefusals(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		r := &fakeRunner{answers: map[string]fakeAnswer{}}
		e := testEnv(t, r)
		e.exe = "/private/var/folders/xy/T/go-build123/b001/exe/mycel"
		m, _ := newManager(goos, e)
		if err := m.Install("node"); err == nil || !strings.Contains(err.Error(), "temporary directory") {
			t.Errorf("%s: install from a temp dir: %v", goos, err)
		}
		e.exe = "mycel"
		m, _ = newManager(goos, e)
		if err := m.Install("node"); err == nil || !strings.Contains(err.Error(), "not absolute") {
			t.Errorf("%s: relative exe: %v", goos, err)
		}
		e.exe = "/usr/local/bin/mycel"
		m, _ = newManager(goos, e)
		if err := m.Install("relay"); err == nil || !strings.Contains(err.Error(), "unknown role") {
			t.Errorf("%s: bad role: %v", goos, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: refusals ran commands: %v", goos, r.calls)
		}
	}
	e := env{exe: `C:\Users\ann\AppData\Local\Temp\mycel.exe`, tempDirs: []string{`C:\Users\ann\AppData\Local\Temp`}}
	if err := e.checkExecutable(); err == nil {
		t.Error("windows temp path accepted")
	}
	e.exe = `c:\users\ann\appdata\local\temp\x\mycel.exe`
	if err := e.checkExecutable(); err == nil {
		t.Error("windows temp path accepted (case)")
	}
	e.exe = `C:\Users\ann\AppData\Local\mycel\bin\mycel.exe`
	if err := e.checkExecutable(); err != nil {
		t.Errorf("installed path refused: %v", err)
	}
	if _, err := newManager("plan9", e); err == nil {
		t.Error("plan9 accepted")
	}
}

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "daemon.log")
	w, err := NewRotatingWriter(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := func(c byte) []byte { return append(bytes.Repeat([]byte{c}, 39), '\n') } // 40 bytes
	for _, c := range []byte("abcdefg") {
		if _, err := w.Write(line(c)); err != nil {
			t.Fatal(err)
		}
	}
	// Writes go a,b | c,d | e,f | g: rotation keeps the newest file and two
	// older ones.
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if got := read(path); got != string(line('g')) {
		t.Errorf("daemon.log = %q", got)
	}
	if got := read(path + ".1"); got != string(line('e'))+string(line('f')) {
		t.Errorf("daemon.log.1 = %q", got)
	}
	if got := read(path + ".2"); got != string(line('c'))+string(line('d')) {
		t.Errorf("daemon.log.2 = %q", got)
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a fourth file exists: %v", err)
	}
	// A second writer (launchd's stdout) appending to the same file is
	// counted and keeps appending after the truncation.
	other, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	_, _ = other.Write(line('x'))
	_, _ = w.Write(line('h')) // 120 > 100: rotate
	_, _ = other.Write(line('y'))
	if got := read(path); got != string(line('h'))+string(line('y')) {
		t.Errorf("after shared rotation daemon.log = %q", got)
	}
	if got := read(path + ".1"); got != string(line('g'))+string(line('x')) {
		t.Errorf("after shared rotation daemon.log.1 = %q", got)
	}
	_ = w.Close()
	if _, err := w.Write([]byte("z")); err == nil {
		t.Error("write after close succeeded")
	}
}

func TestTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, strings.Repeat("x", i%7)+"line")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Tail(context.Background(), path, 3, false, &b); err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(lines[497:], "\n") + "\n"; b.String() != want {
		t.Errorf("tail -n 3 = %q, want %q", b.String(), want)
	}
	b.Reset()
	_ = Tail(context.Background(), path, 1000, false, &b)
	if b.Len() != len(strings.Join(lines, "\n"))+1 {
		t.Errorf("tail -n 1000 printed %d bytes", b.Len())
	}

	// Follow: picks up appends and starts over after a truncation.
	ctx, cancel := context.WithCancel(context.Background())
	pr := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Tail(ctx, path, 0, true, pr) }()
	appendTo := func(s string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	time.Sleep(300 * time.Millisecond) // let Tail read the current end first
	appendTo("new 1\n")
	waitFor(t, func() bool { return strings.Contains(pr.String(), "new 1\n") })
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	appendTo("after rotation\n")
	waitFor(t, func() bool { return strings.Contains(pr.String(), "after rotation\n") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pr.String(), "line") {
		t.Errorf("follow with -n 0 printed old lines: %q", pr.String())
	}

	if err := Tail(context.Background(), filepath.Join(t.TempDir(), "none.log"), 10, false, &b); err == nil || !strings.Contains(err.Error(), "no log yet") {
		t.Errorf("missing log: %v", err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIsDaemonCmdline(t *testing.T) {
	if !isDaemonCmdline([]string{`C:\m\mycel.exe`, "daemon", "--log-file", `C:\l\daemon.log`}) {
		t.Error("daemon command line not matched")
	}
	if isDaemonCmdline([]string{"mycel", "service", "status"}) || isDaemonCmdline([]string{"mycel", "daemon"}) || isDaemonCmdline(nil) {
		t.Error("non-service command line matched")
	}
}

func wantCalls(t *testing.T, r *fakeRunner, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("commands:\n  %s\nwant:\n  %s", strings.Join(r.calls, "\n  "), strings.Join(want, "\n  "))
	}
}
