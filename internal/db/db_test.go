package db

import (
	"context"
	"os"
	"testing"
)

func setupTestDB(t *testing.T) *DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "llmcontrol_test_*.db")
	if err != nil {
		t.Fatalf("failed to create temp db file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	t.Cleanup(func() {
		os.Remove(tmpPath)
	})

	database, err := NewDB(tmpPath)
	if err != nil {
		t.Fatalf("failed to initialize db: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
	})

	return database
}

func TestEnginesCRUD(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	eng := Engine{
		ID:          "llama-vk",
		Name:        "Llama.cpp Vulkan",
		BinaryPath:  "/usr/local/bin/llama-server-vk",
		DefaultArgs: "--vulkan",
	}

	if err := d.SaveEngine(ctx, eng); err != nil {
		t.Fatalf("failed to save engine: %v", err)
	}

	got, err := d.GetEngine(ctx, "llama-vk")
	if err != nil {
		t.Fatalf("failed to get engine: %v", err)
	}
	if got.Name != eng.Name || got.BinaryPath != eng.BinaryPath {
		t.Errorf("expected engine %+v, got %+v", eng, got)
	}

	engines, err := d.ListEngines(ctx)
	if err != nil {
		t.Fatalf("failed to list engines: %v", err)
	}
	if len(engines) != 1 {
		t.Errorf("expected 1 engine, got %d", len(engines))
	}
}

func TestModelsAndProfilesCRUD(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	// Engine
	_ = d.SaveEngine(ctx, Engine{
		ID:         "llama-vk",
		Name:       "Llama Vulkan",
		BinaryPath: "/path/to/llama-server",
	})

	// Model
	m := Model{
		ID:             "gemma4",
		Name:           "Gemma 4 26B",
		EngineID:       "llama-vk",
		ModelPath:      "/models/gemma4.gguf",
		MMProjPath:     "/models/mmproj.gguf",
		MTPPath:        "/models/mtp.gguf",
		DefaultPort:    8088,
		DefaultProfile: "default",
		IsFavorite:     true,
	}

	if err := d.SaveModel(ctx, m); err != nil {
		t.Fatalf("failed to save model: %v", err)
	}

	// Profiles
	p1 := Profile{
		ModelID:     "gemma4",
		Name:        "fast",
		Description: "2K ctx fast",
		CtxSize:     2048,
		KVType:      "q4_0",
		FlashAttn:   "on",
		UseMTP:      true,
		UseVision:   false,
		EnableUI:    true,
		Tools:       "safe",
	}
	p2 := Profile{
		ModelID:     "gemma4",
		Name:        "default",
		Description: "4K ctx default",
		CtxSize:     4096,
		KVType:      "q8_0",
		FlashAttn:   "auto",
		UseMTP:      true,
		UseVision:   false,
		EnableUI:    true,
		Tools:       "safe",
	}
	p3 := Profile{
		ModelID:        "gemma4",
		Name:           "dflash2",
		Description:    "DFlash2 speculative profile",
		CtxSize:        4096,
		KVType:         "q8_0",
		FlashAttn:      "on",
		SpecType:       "draft-dflash",
		DraftModelPath: "/models/dflash2.gguf",
		DraftNMax:      8,
		DraftNGL:       99,
		UseMTP:         false,
		EnableUI:       true,
		Tools:          "safe",
	}

	if err := d.SaveProfile(ctx, p1); err != nil {
		t.Fatalf("failed to save profile 1: %v", err)
	}
	if err := d.SaveProfile(ctx, p2); err != nil {
		t.Fatalf("failed to save profile 2: %v", err)
	}
	if err := d.SaveProfile(ctx, p3); err != nil {
		t.Fatalf("failed to save profile 3: %v", err)
	}

	// Get Model with profiles
	gotModel, err := d.GetModel(ctx, "gemma4")
	if err != nil {
		t.Fatalf("failed to get model: %v", err)
	}
	if gotModel.Name != "Gemma 4 26B" {
		t.Errorf("model name mismatch: %s", gotModel.Name)
	}
	if len(gotModel.Profiles) != 3 {
		t.Fatalf("expected 3 profiles, got %d", len(gotModel.Profiles))
	}
	gotP3, err := d.GetProfile(ctx, "gemma4", "dflash2")
	if err != nil || gotP3 == nil {
		t.Fatalf("failed to get p3 profile: %v", err)
	}
	if gotP3.SpecType != "draft-dflash" || gotP3.DraftModelPath != "/models/dflash2.gguf" || gotP3.DraftNMax != 8 || gotP3.DraftNGL != 99 {
		t.Errorf("unexpected p3 spec fields: %+v", gotP3)
	}

	// Test Favorite Toggle
	if err := d.SetFavorite(ctx, "gemma4", false); err != nil {
		t.Fatalf("failed to toggle favorite: %v", err)
	}
	favs, err := d.ListFavorites(ctx)
	if err != nil {
		t.Fatalf("failed to list favorites: %v", err)
	}
	if len(favs) != 0 {
		t.Errorf("expected 0 favorites after toggle, got %d", len(favs))
	}
}

func TestRuntimeStateAndBenchLogs(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	_ = d.SaveEngine(ctx, Engine{ID: "e1", Name: "E1", BinaryPath: "/bin"})
	_ = d.SaveModel(ctx, Model{ID: "m1", Name: "M1", EngineID: "e1", ModelPath: "/m.gguf"})

	myPID := os.Getpid()
	state := RuntimeState{
		ModelID:     "m1",
		PID:         myPID,
		Port:        8088,
		ProfileName: "fast",
		Status:      "running",
	}

	if err := d.SetRuntimeState(ctx, state); err != nil {
		t.Fatalf("failed to set runtime state: %v", err)
	}

	active, err := d.GetActiveRuntimeStates(ctx)
	if err != nil {
		t.Fatalf("failed to get active states: %v", err)
	}
	if len(active) != 1 || active[0].PID != myPID {
		t.Errorf("unexpected active states: %+v", active)
	}

	// Test dead PID sanitization
	deadState := RuntimeState{
		ModelID:     "m2",
		PID:         999999999, // guaranteed dead PID
		Port:        8089,
		ProfileName: "default",
		Status:      "running",
	}
	_ = d.SaveModel(ctx, Model{ID: "m2", Name: "M2", EngineID: "e1", ModelPath: "/m2.gguf"})
	_ = d.SetRuntimeState(ctx, deadState)

	m2, err := d.GetModel(ctx, "m2")
	if err != nil || m2 == nil {
		t.Fatalf("failed to get m2: %v", err)
	}
	if m2.Runtime.Status != "stopped" {
		t.Errorf("expected dead PID state to be sanitized to 'stopped', got: %s", m2.Runtime.Status)
	}

	// Bench log
	blog := BenchmarkLog{
		ModelID:            "m1",
		ProfileName:        "fast",
		PromptTokens:       10,
		PromptPerSecond:    150.5,
		PredictedTokens:    20,
		PredictedPerSecond: 45.2,
		Success:            true,
		OutputSample:       "7*8=56",
	}
	if err := d.SaveBenchmarkLog(ctx, blog); err != nil {
		t.Fatalf("failed to save bench log: %v", err)
	}

	latest, err := d.GetLatestBenchmark(ctx, "m1")
	if err != nil {
		t.Fatalf("failed to get latest bench: %v", err)
	}
	if latest == nil || latest.PredictedPerSecond != 45.2 {
		t.Errorf("unexpected latest benchmark: %+v", latest)
	}
}
