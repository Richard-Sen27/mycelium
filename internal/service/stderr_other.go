//go:build !windows

package service

// RedirectStderr is a no-op outside Windows: launchd and systemd append the
// daemon's stderr to the log file themselves.
func RedirectStderr(string) error { return nil }
