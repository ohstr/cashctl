//go:build integration

// Package integration is a black-box, end-to-end suite that drives the
// compiled cashctl binary as a real user/agent would, against a real, already-
// running lokihub instance. It is excluded from normal builds/tests by the
// "integration" build tag — run it explicitly with
// `go test -tags integration ./integration/...` (see integration/README.md).
//
// Every fixture a test needs (a cash_hub to mint from, a circle_hub to join)
// is provisioned on demand via lokihub's own admin HTTP API and torn down in
// its own t.Cleanup — config.local.yaml names nothing but that admin API
// itself, mirroring lokihub's own integration suite (which this package's
// admin_client.go is deliberately modeled on for consistency).
package integration

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AdminAPIConfig names the lokihub instance's own admin HTTP API — the ONLY
// thing config.local.yaml needs to name. Every cash_hub/circle_hub fixture
// is provisioned through it at test time (see ephemeral.go) rather than
// hand-set-up beforehand.
//
// Token is a cash-mode JWT, not a long-lived static API key: mint one via
// POST {base_url}/api/unlock (permission: "full", an explicit
// token_expiry_days). Because it expires, operators need to refresh it here
// periodically — an expired token makes every test in this suite fail
// loudly (not skip), since an expired admin token is a config problem, not
// a capability gap.
type AdminAPIConfig struct {
	BaseURL string `yaml:"base_url"`
	Token   string `yaml:"token"`
}

// Config is the top-level shape of config.local.yaml.
type Config struct {
	AdminAPI AdminAPIConfig `yaml:"admin_api"`
}

const defaultConfigPath = "config.local.yaml"

// LoadConfig reads and parses the integration config file. path defaults to
// config.local.yaml (relative to the integration/ package directory) when
// empty; the INTEGRATION_CONFIG env var overrides it.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("INTEGRATION_CONFIG")
	}
	if path == "" {
		path = defaultConfigPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read integration config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse integration config %q: %w", path, err)
	}
	return &cfg, nil
}
