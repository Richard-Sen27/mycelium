package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// hubLockFile is held by a running hub (runHub) and by the offline
// operations that must not run beside it (offline export and redirect).
const hubLockFile = "hub.lock"

// errHubLocked: another process holds hub.lock (normally the running hub).
var errHubLocked = errors.New("hub: the hub is running (hub.lock is held); stop it first, or use the running hub's API")

// lockHubDir takes the exclusive lock on <dir>/hub.lock without waiting;
// unlock releases it.
func lockHubDir(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, hubLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hub: lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errHubLocked) {
			return nil, errHubLocked
		}
		return nil, fmt.Errorf("hub: lock: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// cleanStaleExports removes .export-* scratch directories an interrupted
// export left behind (the lock is held, so none is in use).
func cleanStaleExports(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".export-*"))
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
}
