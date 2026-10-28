package daemon

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// writeStateFile replaces node.yaml atomically.
func writeStateFile(dir string, st State) error {
	b, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, StateFile), b)
}

// writeFileAtomic writes data to a temporary file next to path (mode 0600)
// and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil && !isWindows() {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
