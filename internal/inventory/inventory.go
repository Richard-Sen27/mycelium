// Package inventory probes the machine a node runs on: OS, CPU, RAM, GPUs,
// Docker and Ollama presence, and available sandbox backends.
//
// Contract (owned by the orchestrator): Probe's signature is fixed. Platform
// specifics go behind build tags. Probe must never block for long: external
// commands get a short timeout and missing tools are reported as absent, not
// as errors.
package inventory

import (
	"context"
	"errors"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// ErrNotImplemented marks a stub.
var ErrNotImplemented = errors.New("inventory: not implemented")

// Version is stamped into Inventory.mycel_version; set by the build.
var Version = "dev"

// Probe collects the inventory. It only returns an error when nothing at all
// could be determined.
func Probe(ctx context.Context) (*mycelv1.Inventory, error) { panic(ErrNotImplemented) }

// Hash returns a stable hash of the inventory fields that matter for change
// detection (everything except probed_at).
func Hash(inv *mycelv1.Inventory) string { panic(ErrNotImplemented) }
