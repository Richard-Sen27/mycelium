//go:build windows

package docker

import (
	"fmt"
	"os"
)

// checkSocketDir refuses a socket directory that is a symlink or not a
// directory. Ownership is left to the per-user data directory's ACL.
func checkSocketDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("docker: socket dir: %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("docker: socket dir %s is not a real directory", dir)
	}
	return nil
}
