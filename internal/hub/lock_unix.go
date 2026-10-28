//go:build !windows

package hub

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on f.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // G115: a file descriptor fits in int
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errHubLocked
	}
	return err
}
