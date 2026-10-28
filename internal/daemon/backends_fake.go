//go:build fakesandbox

package daemon

// The CI-only fake sandbox backend (it refuses to run unless
// MYCEL_FAKE_SANDBOX_OK=1).
import _ "github.com/richard-sen27/mycelium/internal/sandbox/fake"
