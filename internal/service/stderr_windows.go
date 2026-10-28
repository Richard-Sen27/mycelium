//go:build windows

package service

import (
	"os"

	"golang.org/x/sys/windows"
)

// RedirectStderr sends this process's standard error (panics, runtime
// errors) to the log file: the scheduled task captures neither stdout nor
// stderr.
func RedirectStderr(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err != nil {
		_ = f.Close()
		return err
	}
	os.Stderr = f
	return nil
}
