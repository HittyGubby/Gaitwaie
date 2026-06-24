package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/HittyGubby/gaitwaie/internal/models"
	"gopkg.in/yaml.v3"
)

// Load reads and parses the YAML config file.
// It fills in default values for optional fields.
func Load(path string) (*models.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg models.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Defaults for optional fields
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(filepath.Dir(path), "gateway.db")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	// Default: strip max-token parameters that cause upstream 400s
	if cfg.StripParams == nil {
		defaults := []string{
			"max_tokens",
			"max_completion_tokens",
			"max_output_tokens",
			"max_gen_tokens",
			"max_new_tokens",
		}
		cfg.StripParams = &defaults
	}

	// Default: auto-disable keys when fail count reaches tolerance
	if cfg.DisableOnTolerance == nil {
		t := true
		cfg.DisableOnTolerance = &t
	}

	// Validate required fields
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("config must have at least one provider")
	}
	if len(cfg.Receivers) == 0 {
		return nil, fmt.Errorf("config must have at least one receiver")
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 3
	}
	if cfg.MaxConcurrentTasks <= 0 {
		cfg.MaxConcurrentTasks = 5
	}

	return &cfg, nil
}

// RemoveProviderKey removes a key from a provider in the YAML config file.
func RemoveProviderKey(path, alias, keyValue string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var cfg models.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	provider, ok := cfg.Providers[alias]
	if !ok {
		return nil
	}

	var newKeys []string
	for _, k := range provider.Keys {
		if k != keyValue {
			newKeys = append(newKeys, k)
		}
	}
	provider.Keys = newKeys
	cfg.Providers[alias] = provider

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, out, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
