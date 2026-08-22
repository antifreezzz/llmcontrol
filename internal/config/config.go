package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	HTTPHost        string `json:"http_host"`
	HTTPPort        int    `json:"http_port"`
	DBPath          string `json:"db_path"`
	LogDir          string `json:"log_dir"`
	ExclusiveMode   bool   `json:"exclusive_mode"`
	TelegramToken   string `json:"telegram_token"`
	TelegramAdminID int64  `json:"telegram_admin_id"`
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".llmcontrol")
	return Config{
		HTTPHost:        "0.0.0.0",
		HTTPPort:        8666,
		DBPath:          filepath.Join(baseDir, "llmcontrol.db"),
		LogDir:          filepath.Join(baseDir, "logs"),
		ExclusiveMode:   true,
		TelegramToken:   "",
		TelegramAdminID: 0,
	}
}

func LoadConfig(customPath string) (Config, error) {
	cfg := DefaultConfig()

	configPath := customPath
	if configPath == "" {
		if _, err := os.Stat("config.json"); err == nil {
			configPath = "config.json"
		} else {
			home, _ := os.UserHomeDir()
			configPath = filepath.Join(home, ".llmcontrol", "config.json")
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			_ = SaveConfig(configPath, cfg)
			return cfg, nil
		}
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}

	// Expand ~ in paths
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(cfg.DBPath, "~/") {
		cfg.DBPath = filepath.Join(home, cfg.DBPath[2:])
	}
	if strings.HasPrefix(cfg.LogDir, "~/") {
		cfg.LogDir = filepath.Join(home, cfg.LogDir[2:])
	}

	// Environment variable overrides
	if envToken := os.Getenv("TELEGRAM_TOKEN"); envToken != "" {
		cfg.TelegramToken = envToken
	}

	return cfg, nil
}

func SaveConfig(configPath string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}
