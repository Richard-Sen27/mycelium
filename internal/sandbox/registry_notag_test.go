//go:build !fakesandbox

package sandbox_test

import (
	"testing"

	"github.com/richard-sen27/mycelium/internal/sandbox"
	_ "github.com/richard-sen27/mycelium/internal/sandbox/fake" // must register nothing without the tag
)

func TestNoBackendsWithoutTag(t *testing.T) {
	if bs := sandbox.Backends(); len(bs) != 0 {
		t.Fatalf("backends registered without build tag fakesandbox: %d", len(bs))
	}
}
