package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// schtasks manages a Task Scheduler task that starts the daemon at the
// user's logon (Windows). It is not a Windows service: the daemon must run
// in the user's interactive session for idle detection and the local token.
//
// The action runs a small .cmd launcher through `conhost.exe --headless`, so
// no console window opens at logon. The launcher sets MYCEL_HOME when it is
// set (Task Scheduler has no per-task environment) and restarts the daemon
// 10 s after it exits, unless it exited 0 (stopped) or 3 (this node was
// revoked). The task's own restart-on-failure covers the launcher.
type schtasks struct{ e env }

// launcherPath is mycel-daemon.cmd next to the logs directory: in
// MYCEL_HOME, or in the data directory.
func (s *schtasks) launcherPath() string {
	return joinPath(parentPath(s.e.logDir), "mycel-daemon.cmd")
}

// parentPath is the directory of p for either separator.
func parentPath(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		return p[:i]
	}
	return p
}

// quoteWinArg quotes one argument for a Windows command line
// (CommandLineToArgvW rules).
func quoteWinArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(c)
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// daemonCommandLine is `"<exe>" daemon --log-file "<log>"`.
func daemonCommandLine(e env) string {
	return quoteWinArg(e.exe) + " daemon --log-file " + quoteWinArg(e.logFile())
}

// cmdEscape escapes a literal for a .cmd file (% expands there).
func cmdEscape(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// renderLauncher renders the .cmd launcher the task runs.
func renderLauncher(e env) string {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("rem Written by `mycel service install`. Remove with `mycel service uninstall`.\r\n")
	b.WriteString("setlocal\r\n")
	if e.home != "" {
		b.WriteString("set \"MYCEL_HOME=" + cmdEscape(e.home) + "\"\r\n")
	}
	b.WriteString(":run\r\n")
	b.WriteString(cmdEscape(daemonCommandLine(e)) + "\r\n")
	b.WriteString("set \"rc=%ERRORLEVEL%\"\r\n")
	b.WriteString("rem 0: stopped; 3: this node was revoked, restarting cannot help.\r\n")
	b.WriteString("if \"%rc%\"==\"0\" exit /b 0\r\n")
	b.WriteString("if \"%rc%\"==\"3\" exit /b 3\r\n")
	b.WriteString("rem wait 10 s (timeout.exe needs a console input)\r\n")
	b.WriteString("ping -n 11 127.0.0.1 >nul\r\n")
	b.WriteString("goto run\r\n")
	return b.String()
}

// taskAction returns the task's command and arguments.
func taskAction(e env, launcher string) (string, string) {
	conhost := joinPath(e.systemRoot, "System32", "conhost.exe")
	cmd := joinPath(e.systemRoot, "System32", "cmd.exe")
	return conhost, "--headless " + quoteWinArg(cmd) + " /d /s /c \"" + quoteWinArg(launcher) + "\""
}

// renderTaskXML renders the task definition (UTF-8 text; schtasks gets it
// as UTF-16, see encodeUTF16).
func renderTaskXML(e env, role, launcher string) string {
	command, args := taskAction(e, launcher)
	user := xmlText(e.username)
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n")
	w("<Task version=\"1.2\" xmlns=\"http://schemas.microsoft.com/windows/2004/02/mit/task\">\n")
	w("  <RegistrationInfo>\n    <Description>Mycelium daemon (mycel, role %s). Written by mycel service install.</Description>\n    <URI>\\%s</URI>\n  </RegistrationInfo>\n", role, TaskName)
	w("  <Triggers>\n    <LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>%s</UserId>\n    </LogonTrigger>\n  </Triggers>\n", user)
	w("  <Principals>\n    <Principal id=\"Author\">\n      <UserId>%s</UserId>\n      <LogonType>InteractiveToken</LogonType>\n      <RunLevel>LeastPrivilege</RunLevel>\n    </Principal>\n  </Principals>\n", user)
	w("  <Settings>\n")
	w("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	w("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	w("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	w("    <AllowHardTerminate>true</AllowHardTerminate>\n")
	w("    <StartWhenAvailable>true</StartWhenAvailable>\n")
	w("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\n")
	w("    <IdleSettings>\n      <StopOnIdleEnd>false</StopOnIdleEnd>\n      <RestartOnIdle>false</RestartOnIdle>\n    </IdleSettings>\n")
	w("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	w("    <Enabled>true</Enabled>\n")
	w("    <Hidden>false</Hidden>\n")
	w("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	w("    <WakeToRun>false</WakeToRun>\n")
	w("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	w("    <Priority>7</Priority>\n")
	w("    <RestartOnFailure>\n      <Interval>PT1M</Interval>\n      <Count>999</Count>\n    </RestartOnFailure>\n")
	w("  </Settings>\n")
	w("  <Actions Context=\"Author\">\n    <Exec>\n      <Command>%s</Command>\n      <Arguments>%s</Arguments>\n    </Exec>\n  </Actions>\n", xmlText(command), xmlText(args))
	w("</Task>\n")
	return b.String()
}

// encodeUTF16 returns s as UTF-16LE with a byte order mark, the encoding
// schtasks /XML reads reliably.
func encodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = binary.LittleEndian.AppendUint16(out, c)
	}
	return out
}

func (s *schtasks) installed() bool {
	_, err := s.e.run.Run("schtasks", "/Query", "/TN", TaskName, "/FO", "CSV", "/NH")
	return err == nil
}

func (s *schtasks) Install(role string) error {
	role, err := checkRole(role)
	if err != nil {
		return err
	}
	if err := s.e.checkExecutable(); err != nil {
		return err
	}
	if s.e.username == "" {
		return errors.New("cannot determine the current user for the logon trigger")
	}
	if err := os.MkdirAll(s.e.logDir, 0o700); err != nil {
		return err
	}
	launcher := s.launcherPath()
	if err := writeFileAtomic(launcher, []byte(renderLauncher(s.e)), 0o600); err != nil {
		return err
	}
	if s.installed() {
		_, _ = s.e.run.Run("schtasks", "/End", "/TN", TaskName)
		s.killLeftover()
	}
	xmlFile := filepath.Join(s.e.logDir, "mycel-task.xml")
	if err := os.WriteFile(xmlFile, encodeUTF16(renderTaskXML(s.e, role, launcher)), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(xmlFile) }()
	if _, err := s.e.run.Run("schtasks", "/Create", "/TN", TaskName, "/XML", xmlFile, "/F"); err != nil {
		return err
	}
	_, err = s.e.run.Run("schtasks", "/Run", "/TN", TaskName)
	return err
}

// killLeftover ends a daemon that outlived its task (/End stops the task's
// own process, conhost): first the launcher loop around it, so it does not
// start the daemon again, then the daemon.
func (s *schtasks) killLeftover() {
	if pid := s.e.findPID(s.e.exe); pid > 0 {
		_ = s.e.killPID(pid)
	}
}

func (s *schtasks) Uninstall() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	_, _ = s.e.run.Run("schtasks", "/End", "/TN", TaskName)
	s.killLeftover()
	if _, err := s.e.run.Run("schtasks", "/Delete", "/TN", TaskName, "/F"); err != nil {
		return err
	}
	if err := os.Remove(s.launcherPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *schtasks) Start() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	_, err := s.e.run.Run("schtasks", "/Run", "/TN", TaskName)
	return err
}

func (s *schtasks) Stop() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	_, err := s.e.run.Run("schtasks", "/End", "/TN", TaskName)
	s.killLeftover()
	return err
}

// Status takes "running" from the process table, not from the task's
// localised status column.
func (s *schtasks) Status() (Status, error) {
	st := Status{Installed: s.installed(), Path: TaskName}
	if !st.Installed {
		return st, nil
	}
	st.PID = s.e.findPID(s.e.exe)
	st.Running = st.PID > 0
	return st, nil
}
