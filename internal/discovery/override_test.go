package discovery

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSetOverrideNilFields(t *testing.T) {
	SetOverride(&Override{})
	t.Cleanup(func() { SetOverride(nil) })
	if _, err := Advertise(context.Background(), Hub{}, AdvertiseOptions{}); !errors.Is(err, errOverridden) {
		t.Fatalf("Advertise with a nil override field: %v", err)
	}
	if hubs, err := Default().Browse(context.Background(), time.Millisecond); len(hubs) != 0 || !errors.Is(err, errOverridden) {
		t.Fatalf("Browse with a nil override field: %v %v", hubs, err)
	}
	SetOverride(nil)
	if _, ok := Default().(mdnsBrowser); !ok {
		t.Fatal("SetOverride(nil) did not restore the mDNS browser")
	}
}
