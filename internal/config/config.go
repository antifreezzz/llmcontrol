package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	HTTPHost           string `json:"http_host"`
	HTTPPort           int    `json:"http_port"`
	DBPath             string `json:"db_path"`
	LogDir             string `json:"log_dir"`
	ModelsDir          string `json:"models_dir"`
	ExclusiveMode      bool   `json:"exclusive_mode"`
	TelegramToken      string `json:"telegram_token"`
	TelegramAdminID    int64  `json:"telegram_admin_id"`
	VPSHost            string `json:"vps_host"`
	VPSTunnelPort      int    `json:"vps_tunnel_port"`
	VPSToken           string `json:"vps_token"`
	VPSRemotePort      int    `json:"vps_remote_port"`
	WhisperBinaryPath  string `json:"whisper_binary_path"`
	WhisperModelPath   string `json:"whisper_model_path"`
	IdleTimeoutSeconds int    `json:"idle_timeout_seconds"`
	WakeOnRequest      bool   `json:"wake_on_request"`
}

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".llmcontrol")
	modelsDir := filepath.Join(home, ".lmstudio", "models")
	return Config{
		HTTPHost:           "0.0.0.0",
		HTTPPort:           8666,
		DBPath:             filepath.Join(baseDir, "llmcontrol.db"),
		LogDir:             filepath.Join(baseDir, "logs"),
		ModelsDir:          modelsDir,
		ExclusiveMode:      true,
		TelegramToken:      "",
		TelegramAdminID:    0,
		VPSHost:            "",
		VPSTunnelPort:      8443,
		VPSToken:           "",
		VPSRemotePort:      8666,
		WhisperBinaryPath:  "/home/antifreezzz/whisper.cpp/build-vk/bin/whisper-cli",
		WhisperModelPath:   "/home/antifreezzz/whisper.cpp/models/ggml-tiny.bin",
		IdleTimeoutSeconds: 300,
		WakeOnRequest:      true,
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
	if envVPSHost := os.Getenv("VPS_HOST"); envVPSHost != "" {
		cfg.VPSHost = envVPSHost
	}
	if envVPSTunnelPort := os.Getenv("VPS_TUNNEL_PORT"); envVPSTunnelPort != "" {
		if p, err := strconv.Atoi(envVPSTunnelPort); err == nil && p > 0 {
			cfg.VPSTunnelPort = p
		}
	}
	if envVPSToken := os.Getenv("VPS_TOKEN"); envVPSToken != "" {
		cfg.VPSToken = envVPSToken
	}
	if envVPSRemotePort := os.Getenv("VPS_REMOTE_PORT"); envVPSRemotePort != "" {
		if p, err := strconv.Atoi(envVPSRemotePort); err == nil && p > 0 {
			cfg.VPSRemotePort = p
		}
	}
	if envWhisperBin := os.Getenv("WHISPER_BINARY_PATH"); envWhisperBin != "" {
		cfg.WhisperBinaryPath = envWhisperBin
	}
	if envWhisperModel := os.Getenv("WHISPER_MODEL_PATH"); envWhisperModel != "" {
		cfg.WhisperModelPath = envWhisperModel
	}
	if envIdleTimeout := os.Getenv("IDLE_TIMEOUT_SECONDS"); envIdleTimeout != "" {
		if s, err := strconv.Atoi(envIdleTimeout); err == nil && s >= 0 {
			cfg.IdleTimeoutSeconds = s
		}
	}
	if envWake := os.Getenv("WAKE_ON_REQUEST"); envWake != "" {
		cfg.WakeOnRequest = strings.ToLower(envWake) == "true" || envWake == "1"
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
