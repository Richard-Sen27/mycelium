//go:build darwin

package sharing

// NewIdleDetector returns the platform detector: ioreg HIDIdleTime on macOS.
func NewIdleDetector() IdleDetector { return newIoregDetector() }
