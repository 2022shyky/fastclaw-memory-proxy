package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen  string   `yaml:"listen"`
	Target  string   `yaml:"target"`
	APIKey  string   `yaml:"api_key"`
	DataDir string   `yaml:"data_dir"`
	Agents  []string `yaml:"agents"`
	Stats   struct {
		Enabled bool   `yaml:"enabled"`
		Output  string `yaml:"output"`
	} `yaml:"stats"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Listen:  ":18954",
		Target:  "http://localhost:18953",
		DataDir: "~/.fastclaw/memory-proxy",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	home, _ := os.UserHomeDir()
	cfg.DataDir = strings.Replace(cfg.DataDir, "~", home, 1)
	if cfg.Stats.Output != "" {
		cfg.Stats.Output = strings.Replace(cfg.Stats.Output, "~", home, 1)
	}

	return cfg, nil
}