//go:build fakesandbox && !unix

package fake

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// termIsKill is true: without signals, TERM terminates the process outright.
const termIsKill = true

// openReadFlags: windows has no O_NOFOLLOW/O_NONBLOCK; the fstat check
// after opening still refuses anything but a regular file.
const openReadFlags = os.O_RDONLY

func setProcAttr(*exec.Cmd) {}

// signalGroup kills the leader for TERM and KILL; STOP and CONT are no-ops.
// Children of the command are not reached on this platform (a Job Object
// will follow with the daemon's windows work).
func signalGroup(p *os.Process, _ int, sig string) error {
	if p == nil {
		return nil
	}
	switch sig {
	case "TERM", "KILL":
		if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	case "STOP", "CONT":
		return nil
	default:
		return errors.New("fake sandbox: unknown signal " + sig)
	}
}

// executable finds p or p with one of the PATHEXT extensions.
func executable(p string) (string, bool) {
	cands := []string{p}
	if filepath.Ext(p) == "" {
		exts := os.Getenv("PATHEXT")
		if exts == "" {
			exts = ".com;.exe;.bat;.cmd"
		}
		for _, e := range strings.Split(exts, ";") {
			if e != "" {
				cands = append(cands, p+strings.ToLower(e))
			}
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() { //nolint:gosec // G703: looking up the job's own command
			return c, true
		}
	}
	return "", false
}

// usage returns CPU milliseconds of an exited process; memory is unknown.
func usage(ps *os.ProcessState) (cpuMilli, maxRSS int64) {
	if ps == nil {
		return 0, 0
	}
	return (ps.UserTime() + ps.SystemTime()).Milliseconds(), 0
}
