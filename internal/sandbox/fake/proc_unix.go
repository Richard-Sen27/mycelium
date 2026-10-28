//go:build fakesandbox && unix

package fake

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// termIsKill is false: unix delivers a real SIGTERM the command may handle.
const termIsKill = false

// openReadFlags opens without following a final symlink and without
// blocking on a FIFO.
const openReadFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW

// setProcAttr puts the command in its own process group so signals reach
// its children too.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends TERM, KILL, STOP or CONT to the process group pgid. A
// group that no longer exists counts as success.
func signalGroup(_ *os.Process, pgid int, sig string) error {
	if pgid <= 0 {
		return nil
	}
	var s syscall.Signal
	switch sig {
	case "TERM":
		s = syscall.SIGTERM
	case "KILL":
		s = syscall.SIGKILL
	case "STOP":
		s = syscall.SIGSTOP
	case "CONT":
		s = syscall.SIGCONT
	default:
		return errors.New("fake sandbox: unknown signal " + sig)
	}
	err := syscall.Kill(-pgid, s)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// executable reports whether p is a regular file with an execute bit.
func executable(p string) (string, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return p, true
}

// usage returns CPU milliseconds and peak RSS bytes of an exited process.
func usage(ps *os.ProcessState) (cpuMilli, maxRSS int64) {
	if ps == nil {
		return 0, 0
	}
	cpuMilli = (ps.UserTime() + ps.SystemTime()).Milliseconds()
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok && ru != nil {
		maxRSS = int64(ru.Maxrss) //nolint:unconvert // int32 on some platforms
		if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
			maxRSS *= 1024 // kilobytes elsewhere
		}
	}
	return cpuMilli, maxRSS
}
