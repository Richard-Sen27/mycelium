package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// launchd manages a per-user launch agent (macOS).
//
// KeepAlive is unconditional: a revoked node exits 3 (not 0), so
// KeepAlive{SuccessfulExit=false} would not stop the restarts either;
// launchd throttles them to one every 10 s and the log says "revoked".
type launchd struct{ e env }

func (l *launchd) plistPath() string {
	return filepath.Join(l.e.userHome, "Library", "LaunchAgents", Label+".plist")
}

func (l *launchd) domain() string  { return "gui/" + strconv.Itoa(l.e.uid) }
func (l *launchd) service() string { return l.domain() + "/" + Label }

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderPlist renders the launch agent definition.
func renderPlist(e env, role string) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	w("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	w("<!-- Written by `mycel service install` (role: %s). Remove with `mycel service uninstall`. -->\n", role)
	w("<plist version=\"1.0\">\n<dict>\n")
	w("\t<key>Label</key>\n\t<string>%s</string>\n", Label)
	w("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range []string{e.exe, "daemon", "--log-file", e.logFile()} {
		w("\t\t<string>%s</string>\n", xmlText(a))
	}
	w("\t</array>\n")
	var keys [][2]string
	if e.home != "" {
		keys = append(keys, [2]string{"MYCEL_HOME", e.home})
	}
	if e.path != "" {
		keys = append(keys, [2]string{"PATH", e.path})
	}
	if len(keys) > 0 {
		w("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, kv := range keys {
			w("\t\t<key>%s</key>\n\t\t<string>%s</string>\n", kv[0], xmlText(kv[1]))
		}
		w("\t</dict>\n")
	}
	w("\t<key>RunAtLoad</key>\n\t<true/>\n")
	w("\t<key>KeepAlive</key>\n\t<true/>\n")
	w("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n")
	w("\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", xmlText(e.logFile()))
	w("\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", xmlText(e.logFile()))
	w("</dict>\n</plist>\n")
	return b.String()
}

func (l *launchd) installed() bool {
	_, err := os.Stat(l.plistPath())
	return err == nil
}

// loaded reports whether launchd knows the agent and returns `launchctl
// print` output.
func (l *launchd) loaded() (string, bool) {
	out, err := l.e.run.Run("launchctl", "print", l.service())
	return out, err == nil
}

// bootstrap loads the plist; right after a bootout launchd may still be
// tearing the old job down, so it retries briefly.
func (l *launchd) bootstrap() error {
	var err error
	for i := 0; i < 5; i++ {
		if _, err = l.e.run.Run("launchctl", "bootstrap", l.domain(), l.plistPath()); err == nil {
			return nil
		}
		l.e.sleep(500 * time.Millisecond)
	}
	return err
}

func (l *launchd) Install(role string) error {
	role, err := checkRole(role)
	if err != nil {
		return err
	}
	if err := l.e.checkExecutable(); err != nil {
		return err
	}
	if err := os.MkdirAll(l.e.logDir, 0o700); err != nil {
		return err
	}
	if _, ok := l.loaded(); ok {
		if _, err := l.e.run.Run("launchctl", "bootout", l.service()); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(l.plistPath(), []byte(renderPlist(l.e, role)), 0o644); err != nil { //nolint:gosec // a launch agent plist holds no secret
		return err
	}
	return l.bootstrap()
}

func (l *launchd) Uninstall() error {
	_, ok := l.loaded()
	if !ok && !l.installed() {
		return ErrNotInstalled
	}
	if ok {
		if _, err := l.e.run.Run("launchctl", "bootout", l.service()); err != nil {
			return err
		}
	}
	if err := os.Remove(l.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *launchd) Start() error {
	if !l.installed() {
		return ErrNotInstalled
	}
	if _, ok := l.loaded(); ok {
		_, err := l.e.run.Run("launchctl", "kickstart", l.service())
		return err
	}
	return l.bootstrap()
}

// Stop unloads the agent (KeepAlive would restart a killed process); it
// stays installed and loads again at the next login or `mycel service start`.
func (l *launchd) Stop() error {
	if !l.installed() {
		return ErrNotInstalled
	}
	if _, ok := l.loaded(); !ok {
		return nil
	}
	_, err := l.e.run.Run("launchctl", "bootout", l.service())
	return err
}

var (
	launchdPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)\s*$`)
	launchdState = regexp.MustCompile(`(?m)^\s*state = (\S+)\s*$`)
)

func (l *launchd) Status() (Status, error) {
	st := Status{Installed: l.installed(), Path: l.plistPath()}
	out, ok := l.loaded()
	if !ok {
		return st, nil
	}
	if m := launchdState.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
	}
	if m := launchdPID.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
		st.Running = st.Running || st.PID > 0
	}
	return st, nil
}
