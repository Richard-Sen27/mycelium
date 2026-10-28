//go:build fakesandbox

package daemon

// The CI-only fake sandbox backend (it refuses to run unless
// MYCEL_FAKE_SANDBOX_OK=1).
import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/richard-sen27/mycelium/internal/sandbox/fake"
)

// fakeGPUTimeout bounds one nvidia-smi run of the fake build's GPU runner.
const fakeGPUTimeout = 10 * time.Second

func init() {
	// With the fake backend switched on, the GPU broker runs the
	// nvidia-smi first on PATH on every OS, macOS included, where the
	// broker is otherwise off: the e2e tests put a fake one there.
	if os.Getenv(fake.EnvOK) == "1" {
		defaultGPURunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, fakeGPUTimeout)
			defer cancel()
			return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // G204: fixed nvidia-smi arguments from the GPU broker
		}
	}
}
