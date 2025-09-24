package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// loadConfig загружает YAML-конфигурацию из файла.
func loadConfig(path string) (*Config, error /* cmd/go-crawler/config.go:12 */) {
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
