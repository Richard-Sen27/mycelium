//go:build linux

// Package firecracker is the placeholder for the Firecracker microVM backend
// (Linux with KVM). It registers itself so `mycel nodes` lists it, but it is
// never available in v1 and every operation returns sandbox.ErrUnavailable.
package firecracker

import (
	"context"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Reason is what Available reports.
const Reason = "not implemented in v1"

type backend struct{}

func init() { sandbox.Register(backend{}) }

func (backend) Name() string { return sandbox.BackendFirecracker }

func (backend) Available(context.Context) (bool, string) { return false, Reason }

func (backend) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: "vm"}
}

func (backend) Prepare(context.Context, string, func(string)) error {
	return sandbox.ErrUnavailable
}

func (backend) Create(context.Context, sandbox.Spec) (sandbox.Sandbox, error) {
	return nil, sandbox.ErrUnavailable
}
