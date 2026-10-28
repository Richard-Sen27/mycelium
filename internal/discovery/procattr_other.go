//go:build !linux

package discovery

import "syscall"

// childAttr: no parent-death signal outside Linux; the advertiser kills the
// child on Close and when its context ends.
func childAttr() *syscall.SysProcAttr { return nil }
