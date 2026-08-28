package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	HTTPHost        string `json:"http_host"`
	HTTPPort        int    `json:"http_port"`
	DBPath          string `json:"db_path"`
	LogDir          string `json:"log_dir"`
	ModelsDir       string `json:"models_dir"`
	ExclusiveMode   bool   `json:"exclusive_mode"`
	TelegramToken   string `json:"telegram_token"`
	TelegramAdminID int64  `json:"telegram_admin_id"`
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".llmcontrol")
	modelsDir := filepath.Join(home, ".lmstudio", "models")
	return Config{
		HTTPHost:        "0.0.0.0",
		HTTPPort:        8666,
		DBPath:          filepath.Join(baseDir, "llmcontrol.db"),
		LogDir:          filepath.Join(baseDir, "logs"),
		ModelsDir:       modelsDir,
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
	if strings.HasPrefix(cfg.ModelsDir, "~/") {
		cfg.ModelsDir = filepath.Join(home, cfg.ModelsDir[2:])
	}

	// Environment variable overrides
	if envToken := os.Getenv("TELEGRAM_TOKEN"); envToken != "" {
		cfg.TelegramToken = envToken
	}
	if envAdminID := os.Getenv("TELEGRAM_ADMIN_ID"); envAdminID != "" {
		if id, err := strconv.ParseInt(envAdminID, 10, 64); err == nil {
			cfg.TelegramAdminID = id
		}
	}
	if envPort := os.Getenv("HTTP_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
			cfg.HTTPPort = p
		}
	}
	if envDBPath := os.Getenv("DB_PATH"); envDBPath != "" {
		cfg.DBPath = envDBPath
	}
	if envLogDir := os.Getenv("LOG_DIR"); envLogDir != "" {
		cfg.LogDir = envLogDir
	}
	if envModelsDir := os.Getenv("MODELS_DIR"); envModelsDir != "" {
		cfg.ModelsDir = envModelsDir
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
