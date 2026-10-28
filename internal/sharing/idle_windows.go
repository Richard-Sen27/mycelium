//go:build windows

package sharing

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	procGetTickCount     = kernel32.NewProc("GetTickCount")

	session0Once sync.Once
)

// lastInputInfo mirrors LASTINPUTINFO.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// NewIdleDetector returns the platform detector: GetLastInputInfo on
// Windows. GetLastInputInfo only sees input of the calling process's own
// session, so the detector must run in the owner's interactive session; in
// session 0 (a Windows service) it logs a warning and reports ErrUnknownIdle (never
// idle), the safe side. The calls return at once, well within IdleTimeout.
func NewIdleDetector() IdleDetector { return lastInputDetector{} }

type lastInputDetector struct{}

func (lastInputDetector) IdleFor() (time.Duration, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		warnNotIdle("ProcessIdToSessionId failed", "err", err)
		return 0, ErrUnknownIdle
	}
	if session == 0 {
		session0Once.Do(func() { warnNotIdle("running in session 0, not in the owner's interactive session") })
		return 0, ErrUnknownIdle
	}
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	r, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))) // #nosec G103 -- Win32 call with a LASTINPUTINFO
	if r == 0 {
		warnNotIdle("GetLastInputInfo failed", "err", err)
		return 0, ErrUnknownIdle
	}
	now, _, _ := procGetTickCount.Call()
	// Both are 32-bit millisecond tick counts; unsigned subtraction handles
	// the 49.7-day wrap.
	return time.Duration(uint32(now)-info.dwTime) * time.Millisecond, nil // #nosec G115 -- GetTickCount returns a DWORD; truncation is intended
}
