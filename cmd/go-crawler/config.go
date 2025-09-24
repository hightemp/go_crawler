package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// loadConfig loads YAML configuration from a file.
func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
