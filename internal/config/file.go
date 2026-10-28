package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// File is the per-machine config file. It holds what the CLI needs to talk to
// a hub. Node and hub role state live in the data directory, not here.
type File struct {
	HubURL string `yaml:"hub_url,omitempty"`
	// APIKey is the user's credential ("mck_..."). Treated as a secret: the
	// file is written with mode 0600 and the value is never logged.
	APIKey string `yaml:"api_key,omitempty"`
	// CACertPEM pins the hub CA for TLS verification.
	CACertPEM string `yaml:"ca_cert_pem,omitempty"`
	// NodeName is set once this machine has joined as a node.
	NodeName string `yaml:"node_name,omitempty"`
	// RedirectIssuedAt is the issued_at of the last signed hub redirect the
	// CLI followed; older ones are refused (phase 7).
	RedirectIssuedAt int64 `yaml:"redirect_issued_at,omitempty"`
}

// Load reads the config file. A missing file yields an empty File and no error.
func Load(d Dirs) (*File, error) {
	var f File
	b, err := os.ReadFile(d.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return &f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", d.ConfigFile(), err)
	}
	return &f, nil
}

// Save writes the config file atomically with mode 0600.
func Save(d Dirs, f *File) error {
	if err := EnsureDir(d.Config); err != nil {
		return err
	}
	b, err := yaml.Marshal(f) //nolint:gosec // this file is the credential store; written 0600
	if err != nil {
		return err
	}
	tmp := filepath.Join(d.Config, ".config.yaml.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.ConfigFile())
}
