//go:build fakesandbox

package inventory

// Built with -tags fakesandbox: advertise the fake (CI test) backend.
func init() { fakeSandbox = true }
