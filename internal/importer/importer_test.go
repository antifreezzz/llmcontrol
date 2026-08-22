package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

func TestParseScript(t *testing.T) {
	scriptContent := `#!/usr/bin/env bash
# Запуск llama-server с Gemma 4 26B A4B QAT (Q4_0) на Vulkan.
#
LLAMA_BIN=/home/antifreezzz/llama.cpp/build-vk/bin/llama-server
MODEL_DIR=/home/antifreezzz/.lmstudio/models/lmstudio-community/gemma-4-26B-A4B-it-QAT-GGUF
MODEL="$MODEL_DIR/gemma-4-26B-A4B-it-QAT-Q4_0.gguf"
MMPROJ="$MODEL_DIR/mmproj-gemma-4-26B-A4B-it-QAT-BF16.gguf"
MTP=/home/antifreezzz/.lmstudio/models/Janvitos/gemma-4-assistant-MTP-Q8_0.gguf
PORT=8088

declare -A PROFILES
PROFILES[fast]="_apply 2048 1 1 0 1 safe q4_0 on"
PROFILES[default]="_apply 4096 1 1 0 1 safe q8_0 auto"
PROFILES[long]="_apply 16384 1 1 0 1 safe q4_0 on"

declare -A PROFILE_DESCR
PROFILE_DESCR[fast]="ctx 2K, MTP, q4_0 KV, flash attn"
PROFILE_DESCR[default]="ctx 4K, MTP, q8_0 KV, WebUI + safe tools"
`

	parsed, err := ParseScriptContent("gemma4-start", scriptContent)
	if err != nil {
		t.Fatalf("failed to parse script: %v", err)
	}

	if parsed.ModelID != "gemma4" {
		t.Errorf("expected modelID gemma4, got %s", parsed.ModelID)
	}
	if parsed.DefaultPort != 8088 {
		t.Errorf("expected port 8088, got %d", parsed.DefaultPort)
	}
	if parsed.EngineBinary != "/home/antifreezzz/llama.cpp/build-vk/bin/llama-server" {
		t.Errorf("unexpected engine binary: %s", parsed.EngineBinary)
	}
	if parsed.ModelPath != "/home/antifreezzz/.lmstudio/models/lmstudio-community/gemma-4-26B-A4B-it-QAT-GGUF/gemma-4-26B-A4B-it-QAT-Q4_0.gguf" {
		t.Errorf("unexpected model path: %s", parsed.ModelPath)
	}
	if parsed.MMProjPath != "/home/antifreezzz/.lmstudio/models/lmstudio-community/gemma-4-26B-A4B-it-QAT-GGUF/mmproj-gemma-4-26B-A4B-it-QAT-BF16.gguf" {
		t.Errorf("unexpected mmproj path: %s", parsed.MMProjPath)
	}
	if parsed.MTPPath != "/home/antifreezzz/.lmstudio/models/Janvitos/gemma-4-assistant-MTP-Q8_0.gguf" {
		t.Errorf("unexpected mtp path: %s", parsed.MTPPath)
	}
	if len(parsed.Profiles) != 3 {
		t.Fatalf("expected 3 profiles, got %d", len(parsed.Profiles))
	}
	if parsed.Profiles["fast"].CtxSize != 2048 {
		t.Errorf("expected fast profile ctx 2048, got %d", parsed.Profiles["fast"].CtxSize)
	}
}

func TestImportDirectory(t *testing.T) {
	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("", "llmcontrol_importer_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbFile := filepath.Join(tmpDir, "test.db")
	database, err := db.NewDB(dbFile)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer database.Close()

	scriptPath := filepath.Join(tmpDir, "cyber-start")
	scriptContent := `#!/usr/bin/env bash
# Qwen3.8 9B Cyber Exploit
LLAMA_BIN="/home/antifreezzz/llama.cpp/build-vk/bin/llama-server"
MODEL="/models/cyber.gguf"
PORT=8080

declare -A PROFILES
PROFILES[fast]="_apply 8192 1 1 '' f16 on 2048 99"
PROFILES[default]="_apply 262144 1 1 '' q4_0 on 2048 99"
`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	res, err := ImportDirectory(ctx, database, tmpDir)
	if err != nil {
		t.Fatalf("failed to import dir: %v", err)
	}
	if res.ImportedModels != 1 {
		t.Errorf("expected 1 imported model, got %d", res.ImportedModels)
	}

	m, err := database.GetModel(ctx, "cyber")
	if err != nil || m == nil {
		t.Fatalf("failed to query imported model from db: %v", err)
	}
	if m.Name == "" || len(m.Profiles) != 2 {
		t.Errorf("unexpected model structure: %+v", m)
	}
}
