//go:build fakesandbox

package main

// The CI-only fake backend; it also needs MYCEL_FAKE_SANDBOX_OK=1 at runtime.
import _ "github.com/richard-sen27/mycelium/internal/sandbox/fake"
