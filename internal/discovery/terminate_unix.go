//go:build !windows

package discovery

import "syscall"

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
