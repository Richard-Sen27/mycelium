package discovery

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Override replaces mDNS for a whole process: Advertise and Default use it
// instead of the network. Only the test build of the binary installs one
// (internal/discovery/fakemdns, linked in with the fakeharness build tag),
// so multi-process tests never touch the network.
type Override struct {
	Advertise func(ctx context.Context, h Hub, opts AdvertiseOptions) (Advertiser, error)
	Browser   func() Browser
}

var override atomic.Pointer[Override]

// errOverridden answers a function an installed Override leaves nil: an
// override never falls back to the network.
var errOverridden = errors.New("discovery: mDNS is replaced in this process and this function is not provided")

// SetOverride installs o (nil removes it). A nil field of o disables that
// half (advertising fails, browsing finds nothing) instead of reaching
// the network. Release builds never call it.
func SetOverride(o *Override) {
	if o == nil {
		override.Store(nil)
		return
	}
	c := *o
	if c.Advertise == nil {
		c.Advertise = func(context.Context, Hub, AdvertiseOptions) (Advertiser, error) { return nil, errOverridden }
	}
	if c.Browser == nil {
		c.Browser = func() Browser { return offBrowser{} }
	}
	override.Store(&c)
}

func loadOverride() *Override { return override.Load() }

type offBrowser struct{}

func (offBrowser) Browse(context.Context, time.Duration) ([]Hub, error) { return nil, errOverridden }
