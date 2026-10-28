package discovery

import "syscall"

// childAttr ends the registration process with the hub, even when the hub
// dies without closing the advertiser.
func childAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM} }
