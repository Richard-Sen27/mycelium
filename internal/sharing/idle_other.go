//go:build !darwin && !linux && !windows

package sharing

// NewIdleDetector returns the platform detector; this platform has none, so
// the owner is never idle.
func NewIdleDetector() IdleDetector { return nopDetector{} }
