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
