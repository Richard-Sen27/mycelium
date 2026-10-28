package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemd manages a systemd user unit (Linux).
type systemd struct{ e env }

func (s *systemd) unitPath() string {
	return filepath.Join(s.e.configHome, "systemd", "user", UnitName)
}

func (s *systemd) systemctl(args ...string) (string, error) {
	return s.e.run.Run("systemctl", append([]string{"--user"}, args...)...)
}

// systemdQuote quotes one word of ExecStart= (specifiers and variables are
// expanded there, so % and $ are doubled).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// systemdEnv quotes one Environment= assignment (no variable expansion).
func systemdEnv(k, v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + k + "=" + r.Replace(v) + `"`
}

// renderUnit renders the user unit.
func renderUnit(e env, role string) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	logFile := strings.ReplaceAll(e.logFile(), "%", "%%")
	w("# Written by `mycel service install` (role: %s). Remove with `mycel service uninstall`.\n", role)
	w("[Unit]\nDescription=Mycelium daemon (mycel, role %s)\nStartLimitIntervalSec=0\n\n", role)
	w("[Service]\nType=simple\n")
	w("ExecStart=%s daemon --log-file %s\n", systemdQuote(e.exe), systemdQuote(e.logFile()))
	if e.home != "" {
		w("Environment=%s\n", systemdEnv("MYCEL_HOME", e.home))
	} else {
		// The daemon must resolve the same directories as the CLI did.
		for _, kv := range e.xdg {
			w("Environment=%s\n", systemdEnv(kv[0], kv[1]))
		}
	}
	if e.path != "" {
		w("Environment=%s\n", systemdEnv("PATH", e.path))
	}
	w("Restart=on-failure\nRestartSec=5\n")
	w("# exit 3: this node was revoked; restarting cannot help\nRestartPreventExitStatus=3\n")
	w("# append: needs systemd 240 or newer\n")
	w("StandardOutput=append:%s\nStandardError=append:%s\n\n", logFile, logFile)
	w("[Install]\nWantedBy=default.target\n")
	return b.String()
}

func (s *systemd) installed() bool {
	_, err := os.Stat(s.unitPath())
	return err == nil
}

func (s *systemd) Install(role string) error {
	role, err := checkRole(role)
	if err != nil {
		return err
	}
	if err := s.e.checkExecutable(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.e.logDir, 0o700); err != nil {
		return err
	}
	reinstall := s.installed()
	if err := writeFileAtomic(s.unitPath(), []byte(renderUnit(s.e, role)), 0o644); err != nil { //nolint:gosec // a unit file holds no secret
		return err
	}
	if _, err := s.systemctl("daemon-reload"); err != nil {
		return err
	}
	if _, err := s.systemctl("enable", "--now", UnitName); err != nil {
		return err
	}
	if reinstall {
		// enable --now leaves a running old binary alone.
		if _, err := s.systemctl("restart", UnitName); err != nil {
			return err
		}
	}
	return nil
}

// Notes is the linger hint: without it the user manager, and the daemon,
// stop when the user's last session ends.
func (s *systemd) Notes() []string {
	return []string{"To keep mycel running while you are logged out: loginctl enable-linger $USER"}
}

func (s *systemd) Uninstall() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	if _, err := s.systemctl("disable", "--now", UnitName); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

func (s *systemd) Start() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	_, err := s.systemctl("start", UnitName)
	return err
}

func (s *systemd) Stop() error {
	if !s.installed() {
		return ErrNotInstalled
	}
	_, err := s.systemctl("stop", UnitName)
	return err
}

func (s *systemd) Status() (Status, error) {
	st := Status{Installed: s.installed(), Path: s.unitPath()}
	if !st.Installed {
		return st, nil
	}
	// is-active exits non-zero for anything but "active"; the output is
	// what matters.
	out, _ := s.systemctl("is-active", UnitName)
	st.Running = strings.TrimSpace(out) == "active"
	if st.Running {
		out, err := s.systemctl("show", UnitName, "--property=MainPID", "--value")
		if err != nil {
			return st, err
		}
		st.PID, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	return st, nil
}
