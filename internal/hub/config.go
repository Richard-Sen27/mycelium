package hub

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// File names under the hub dir.
const (
	caCertFile      = "ca.crt"
	caKeyFile       = "ca.key"
	serverCertFile  = "server.crt"
	serverKeyFile   = "server.key"
	agentKeyFile    = "agent-token.key"
	hubConfigFile   = "hub.yaml"
	dbFile          = "hub.db"
	jetStreamSubdir = "jetstream"
)

// Config is hub.yaml: the hub's fixed identity and listen addresses.
type Config struct {
	TrustDomain  string `yaml:"trust_domain"`
	AdvertiseURL string `yaml:"advertise_url"`
	BusAdvertise string `yaml:"bus_advertise"`
	APIListen    string `yaml:"api_listen"`
	BusListen    string `yaml:"bus_listen"`
	CreatedAt    string `yaml:"created_at"`
}

// ReadConfig reads hub.yaml from hubDir.
func ReadConfig(hubDir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(hubDir, hubConfigFile))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("hub: parse %s: %w", hubConfigFile, err)
	}
	return &c, nil
}

func writeConfig(hubDir string, c *Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hubDir, hubConfigFile), b, 0o600)
}
