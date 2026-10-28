//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

// canPause reports whether pause actually suspends the process.
const canPause = true

// setProcAttrs starts the worker in its own process group, so signals reach
// the harness and everything it starts (its MCP server, tools).
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// procCtl controls a worker's process group. Callers hold worker.mu and
// check that the leader has not been reaped yet.
type procCtl struct{ pgid int }

func newProcCtl(p *os.Process) (*procCtl, error) { return &procCtl{pgid: p.Pid}, nil }

func (c *procCtl) signal(sig syscall.Signal) error { return syscall.Kill(-c.pgid, sig) }

// pause suspends the process group.
func (c *procCtl) pause() error { return c.signal(syscall.SIGSTOP) }

// resume continues the process group.
func (c *procCtl) resume() error { return c.signal(syscall.SIGCONT) }

// terminate asks the process group to exit. A stopped group is continued
// so it can act on SIGTERM.
func (c *procCtl) terminate() error {
	err := c.signal(syscall.SIGTERM)
	_ = c.signal(syscall.SIGCONT)
	return err
}

// kill kills the process group.
func (c *procCtl) kill() error { return c.signal(syscall.SIGKILL) }

// release frees resources held for the group (none on unix).
func (c *procCtl) release() {}
