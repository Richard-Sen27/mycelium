package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// File names under NodeDir.
const (
	StateFile = "node.yaml"
	KeyFile   = "node.key"
	CertFile  = "node.crt"
	CAFile    = "ca.crt"
)

// State is node.yaml: everything a node needs to find and trust its hub.
// Certificates and the key live next to it as PEM files.
type State struct {
	Name        string   `yaml:"name"`
	SPIFFEID    string   `yaml:"spiffe_id"`
	TrustDomain string   `yaml:"trust_domain"`
	HubURL      string   `yaml:"hub_url"`
	BusURL      string   `yaml:"bus_url"`
	OwnerID     string   `yaml:"owner_id"`
	Caps        []string `yaml:"caps"`
}

// ErrNoNode is returned by ReadState when NodeDir holds no node.
var ErrNoNode = errors.New("daemon: no node state in directory")

// ErrNodeExists is returned by WriteState and Join when NodeDir already holds a node.
var ErrNodeExists = errors.New("daemon: node already joined")

// WriteState creates NodeDir (0700) and writes node.yaml, node.key (0600),
// node.crt and ca.crt. It refuses to overwrite an existing node.yaml.
func WriteState(nodeDir string, st State, keyPEM, certPEM, caPEM []byte) error {
	if _, err := os.Stat(filepath.Join(nodeDir, StateFile)); err == nil {
		return ErrNodeExists
	}
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(nodeDir, KeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(nodeDir, CertFile), certPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(nodeDir, CAFile), caPEM, 0o600); err != nil {
		return err
	}
	b, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(nodeDir, StateFile), b, 0o600)
}

// ReadState reads node.yaml.
func ReadState(nodeDir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(nodeDir, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoNode
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := yaml.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", StateFile, err)
	}
	return &st, nil
}

// HasState reports whether NodeDir holds a node.
func HasState(nodeDir string) bool {
	_, err := os.Stat(filepath.Join(nodeDir, StateFile))
	return err == nil
}
