//go:build windows

package daemon

// Windows worker control. Compile-checked with GOOS=windows only: this code
// has not yet run on a Windows test runner.

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// canPause is false: Windows has no SIGSTOP; a pause only refuses the
// worker's local RPCs.
const canPause = false

// setProcAttrs starts the worker in its own process group.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// procCtl holds the worker in a Job Object with KILL_ON_JOB_CLOSE, so the
// harness and everything it starts end together, even if the daemon dies.
// Processes the harness starts before it is assigned to the job (a few
// instructions after start) are not covered.
type procCtl struct {
	p   *os.Process
	job windows.Handle
}

func newProcCtl(p *os.Process) (*procCtl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { // #nosec G103 G115 -- Win32 API struct
		_ = windows.CloseHandle(job)
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid)) // #nosec G115 -- a pid
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &procCtl{p: p, job: job}, nil
}

func (c *procCtl) pause() error  { return nil }
func (c *procCtl) resume() error { return nil }

// terminate ends the job: Windows has no graceful signal for a windowless
// child.
func (c *procCtl) terminate() error { return c.kill() }

func (c *procCtl) kill() error {
	if c.job == 0 {
		return c.p.Kill()
	}
	err := windows.TerminateJobObject(c.job, 1)
	if err != nil {
		err = errors.Join(err, c.p.Kill())
	}
	return err
}

// release closes the job handle, which kills anything left in it.
func (c *procCtl) release() {
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
}
