package service

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// findDaemonPID returns the pid of a running `mycel daemon --log-file …`
// started from exe, or 0. Used where the service manager does not report a
// pid (Task Scheduler).
func findDaemonPID(exe string) int {
	procs, err := process.Processes()
	if err != nil {
		return 0
	}
	self := int32(os.Getpid()) //nolint:gosec // pids fit in int32
	for _, p := range procs {
		if p.Pid == self {
			continue
		}
		args, err := p.CmdlineSlice()
		if err != nil || !isDaemonCmdline(args) {
			continue
		}
		pexe, err := p.Exe()
		if err != nil || !samePath(pexe, exe) {
			continue
		}
		return int(p.Pid)
	}
	return 0
}

// isDaemonCmdline matches the command line the service units run.
func isDaemonCmdline(args []string) bool {
	hasDaemon, hasLog := false, false
	for _, a := range args[min(1, len(args)):] {
		switch a {
		case "daemon":
			hasDaemon = true
		case "--log-file":
			hasLog = true
		}
	}
	return hasDaemon && hasLog
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if strings.EqualFold(a, b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && strings.EqualFold(ra, rb)
}

// killPID ends the daemon and, first, the launcher loop (cmd.exe running
// mycel-daemon.cmd) that would otherwise start it again.
func killPID(pid int) error {
	p, err := process.NewProcess(int32(pid)) //nolint:gosec // pids fit in int32
	if err != nil {
		return err
	}
	if ppid, err := p.Ppid(); err == nil && ppid > 0 {
		if parent, err := process.NewProcess(ppid); err == nil {
			if name, err := parent.Name(); err == nil && strings.EqualFold(name, "cmd.exe") {
				if args, err := parent.Cmdline(); err == nil && strings.Contains(strings.ToLower(args), "mycel-daemon.cmd") {
					_ = parent.Kill()
				}
			}
		}
	}
	return p.Kill()
}
