package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/richard-sen27/mycelium/internal/policy"
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
	// Grants holds the auto-approval rules (phase 5).
	Grants GrantsConfig `yaml:"grants,omitempty"`
}

// GrantsConfig is the grants section of hub.yaml.
type GrantsConfig struct {
	// Auto rules, first match wins (policy.GrantRule). Empty channel, kind
	// or node mean "*".
	Auto []GrantRuleConfig `yaml:"auto,omitempty"`
	// PendingTTL is how long a request waits for a decision before it
	// expires; default 24h.
	PendingTTL string `yaml:"pending_ttl,omitempty"`
}

// GrantRuleConfig is one entry of grants.auto.
type GrantRuleConfig struct {
	Channel  string `yaml:"channel,omitempty"`
	Kind     string `yaml:"kind,omitempty"`
	Node     string `yaml:"node,omitempty"`
	MaxTTL   string `yaml:"max_ttl,omitempty"`
	MaxCount int    `yaml:"max_count,omitempty"`
	// ReadOnly defaults to true: the rule matches only read-only mounts.
	// Read-write mounts auto-approve only under a rule that says
	// read_only: false explicitly.
	ReadOnly *bool `yaml:"read_only,omitempty"`
}

// maxGrantRules bounds grants.auto.
const maxGrantRules = 256

// grantConfig returns the grant timings with hub.yaml's pending_ttl.
func (c *Config) grantConfig() (grantConfig, error) {
	out := defaultGrantConfig
	if v := c.Grants.PendingTTL; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 30*24*time.Hour {
			return out, fmt.Errorf("hub: grants.pending_ttl %q must be a duration up to 720h", v)
		}
		out.pendingTTL = d
	}
	return out, nil
}

// GrantRules validates grants.auto and returns it as policy rules.
func (c *Config) GrantRules() ([]policy.GrantRule, error) {
	if len(c.Grants.Auto) > maxGrantRules {
		return nil, fmt.Errorf("hub: grants.auto: at most %d rules", maxGrantRules)
	}
	out := make([]policy.GrantRule, 0, len(c.Grants.Auto))
	for i, r := range c.Grants.Auto {
		kind := orDefault(r.Kind, "*")
		if kind != "*" && !slices.Contains(grantKinds, kind) {
			return nil, fmt.Errorf("hub: grants.auto[%d]: unknown kind %q (known: ollama, gpu, docker-host, mount, egress, *)", i, r.Kind)
		}
		if r.MaxTTL != "" {
			d, err := time.ParseDuration(r.MaxTTL)
			if err != nil || d <= 0 || d > policy.MaxGrantTTL {
				return nil, fmt.Errorf("hub: grants.auto[%d]: max_ttl %q must be a duration up to %s", i, r.MaxTTL, policy.MaxGrantTTL)
			}
		}
		if r.MaxCount < 0 {
			return nil, fmt.Errorf("hub: grants.auto[%d]: max_count must not be negative", i)
		}
		out = append(out, policy.GrantRule{
			Channel: orDefault(r.Channel, "*"), Kind: kind, Node: orDefault(r.Node, "*"),
			MaxTTL: r.MaxTTL, MaxCount: r.MaxCount, ReadOnly: r.ReadOnly == nil || *r.ReadOnly,
		})
	}
	return out, nil
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
