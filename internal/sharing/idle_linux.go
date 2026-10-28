//go:build linux

package sharing

// NewIdleDetector returns the platform detector: logind's idle hint for
// the active session, else xprintidle, else 0 (never idle) on Linux.
func NewIdleDetector() IdleDetector { return newLinuxDetector() }
