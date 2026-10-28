//go:build !windows

package docker

import (
	"fmt"
	"os"
	"syscall"
)

// checkSocketDir refuses a socket directory that is a symlink, not a
// directory, or not owned by the daemon's user.
func checkSocketDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("docker: socket dir: %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("docker: socket dir %s is not a real directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("docker: socket dir %s is owned by uid %d, not %d", dir, sys.Uid, os.Getuid())
	}
	return nil
}
