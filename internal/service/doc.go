// Package service installs the daemon as a per-user background service so
// a machine stays a node after the terminal closes (spec "One install,
// every role"; phase 7, owned by P7-install).
//
// `mycel service install [--role node|hub]`, `uninstall`, `start`, `stop`,
// `status`, `logs [-f]`:
//
//   - macOS: a launchd user agent, ~/Library/LaunchAgents/io.mycel.daemon.plist,
//     RunAtLoad and KeepAlive, running `mycel daemon` (or `mycel hub serve`)
//     with MYCEL_HOME set, stdout/stderr to <MYCEL_HOME>/logs/daemon.log.
//   - Linux: a systemd user unit, ~/.config/systemd/user/mycel.service,
//     enabled; `install` prints the `loginctl enable-linger` hint so the
//     unit runs without an open session.
//   - Windows: a Scheduled Task at user logon (not a Windows service: the
//     daemon must run in the user's interactive session for idle detection
//     and the local token), highest run level off, restart on failure.
//
// The binary path recorded in the unit is the running executable's path;
// `install` refuses a path inside a temporary directory. `status` reports
// installed, running and the pid; `logs` tails the log file with rotation
// at 10 MiB x 3 handled by the daemon (`mycel daemon --log-file`, see
// RotatingWriter). Every command works without
// elevation; nothing touches system-wide locations.
package service

// Status is what `mycel service status` reports.
type Status struct {
	Installed bool
	Running   bool
	PID       int
	Path      string // the unit, plist or task name
}

// Manager is implemented per platform.
type Manager interface {
	Install(role string) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
}
