// Package config resolves the mycel config and data directories and reads and
// writes the per-machine config file.
//
// Layout (see docs/decisions.md D007):
//
//	<config>/config.yaml        CLI credentials and hub address
//	<data>/hub/                 hub role state (created by `mycel hub init`)
//	<data>/node/                node role state (created by `mycel join`)
//
// MYCEL_HOME=<dir> overrides both: <dir>/config.yaml and <dir>/data/.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// EnvHome is the environment variable that overrides both directories.
const EnvHome = "MYCEL_HOME"

const appDir = "mycel"

// Dirs holds the resolved directories for this process.
type Dirs struct {
	Config string // directory holding config.yaml
	Data   string // directory holding hub/ and node/
}

// ConfigFile returns the path of the config file.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.yaml") }

// HubDir returns the hub role's state directory.
func (d Dirs) HubDir() string { return filepath.Join(d.Data, "hub") }

// NodeDir returns the node role's state directory.
func (d Dirs) NodeDir() string { return filepath.Join(d.Data, "node") }

// Resolve returns the directories, honouring MYCEL_HOME. It does not create them.
func Resolve() (Dirs, error) {
	if home := os.Getenv(EnvHome); home != "" {
		abs, err := filepath.Abs(home)
		if err != nil {
			return Dirs{}, err
		}
		return Dirs{Config: abs, Data: filepath.Join(abs, "data")}, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return Dirs{}, err
	}
	data, err := userDataDir()
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{Config: filepath.Join(cfg, appDir), Data: filepath.Join(data, appDir)}, nil
}

// MustResolve is Resolve for callers that treat failure as fatal.
func MustResolve() Dirs {
	d, err := Resolve()
	if err != nil {
		panic(err)
	}
	return d
}

func userDataDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v, nil
		}
		return "", errors.New("LOCALAPPDATA is not set")
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// EnsureDir creates dir with mode 0700 if it does not exist.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
