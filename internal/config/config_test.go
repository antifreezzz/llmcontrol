package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoadSave(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "llmctl_cfg_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("failed to load default config: %v", err)
	}

	if cfg.HTTPPort != 8666 {
		t.Errorf("expected default port 8666, got %d", cfg.HTTPPort)
	}

	cfg.TelegramToken = "123456:ABC-DEF"
	cfg.TelegramAdminID = 987654321
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	if loaded.TelegramToken != "123456:ABC-DEF" || loaded.TelegramAdminID != 987654321 {
		t.Errorf("loaded config mismatch: %+v", loaded)
	}
}
