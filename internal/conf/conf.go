package conf

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"

	"github.com/ismdeep/log"
)

type Config struct {
	ServerAddr string `yaml:"server_addr" json:"server_addr"`
}

//go:embed config.example.json
var defaultContent []byte

func Load() (Config, error) {
	if content, err := os.ReadFile("config.json"); err != nil {
		log.WithContext(context.Background()).Warn("failed to load config.json, use default instead.")
	} else {
		defaultContent = content
	}

	var cfg Config
	if err := json.Unmarshal(defaultContent, &cfg); err != nil {
		return Config{}, nil
	}

	return cfg, nil
}
