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
	if len(engines) < 1 {
		t.Errorf("expected at least 1 engine, got %d", len(engines))
	}
}

func TestDiffusionModelAndGeneratedImagesCRUD(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	// Save Diffusion Engine
	_ = d.SaveEngine(ctx, Engine{
		ID:          "sd-vk",
		Name:        "Stable Diffusion Vulkan",
		BinaryPath:  "/path/to/sd-cli",
		DefaultArgs: "--mode img_gen",
	})

	// Save Diffusion Model
	m := Model{
		ID:             "z-image-turbo",
		Name:           "Z-Image-Turbo (GGUF Q4_K_M)",
		EngineID:       "sd-vk",
		ModelType:      "diffusion",
		ModelPath:      "/models/z-image-turbo-Q4_K_M.gguf",
		VAEPath:        "/models/ae.safetensors",
		CLIPPath:       "/models/qwen_3_4b.safetensors",
		DefaultProfile: "default",
	}
	if err := d.SaveModel(ctx, m); err != nil {
		t.Fatalf("failed to save diffusion model: %v", err)
	}

	p := Profile{
		ModelID:        "z-image-turbo",
		Name:           "default",
		Description:    "1024x1024 Turbo with CPU Offload",
		ClipOnCPU:      true,
		VAEOnCPU:       false,
		Width:          1024,
		Height:         1024,
		Steps:          4,
		CFGScale:       1.0,
		SamplingMethod: "euler",
		Tools:          "safe",
	}
	if err := d.SaveProfile(ctx, p); err != nil {
		t.Fatalf("failed to save diffusion profile: %v", err)
	}

	gotModel, err := d.GetModel(ctx, "z-image-turbo")
	if err != nil || gotModel == nil {
		t.Fatalf("failed to get diffusion model: %v", err)
	}
	if gotModel.ModelType != "diffusion" || gotModel.VAEPath != "/models/ae.safetensors" || gotModel.CLIPPath != "/models/qwen_3_4b.safetensors" {
		t.Errorf("unexpected diffusion model fields: %+v", gotModel)
	}

	gotProfile, err := d.GetProfile(ctx, "z-image-turbo", "default")
	if err != nil || gotProfile == nil {
		t.Fatalf("failed to get diffusion profile: %v", err)
	}
	if !gotProfile.ClipOnCPU || gotProfile.Width != 1024 || gotProfile.Steps != 4 || gotProfile.SamplingMethod != "euler" {
		t.Errorf("unexpected diffusion profile fields: %+v", gotProfile)
	}

	// Generated Images CRUD
	img := GeneratedImage{
		ID:             "img_123456",
		Prompt:         "a cute cat in cyberpunk style",
		NegativePrompt: "blurry, low quality",
		ModelID:        "z-image-turbo",
		ProfileName:    "default",
		Width:          1024,
		Height:         1024,
		Steps:          4,
		CFGScale:       1.0,
		Seed:           42,
		DurationMs:     35200,
		FilePath:       "/home/user/.llmcontrol/images/img_123456.png",
	}

	if err := d.SaveGeneratedImage(ctx, img); err != nil {
		t.Fatalf("failed to save generated image: %v", err)
	}

	gotImg, err := d.GetGeneratedImage(ctx, "img_123456")
	if err != nil || gotImg == nil {
		t.Fatalf("failed to get generated image: %v", err)
	}
	if gotImg.Prompt != img.Prompt || gotImg.Width != 1024 || gotImg.DurationMs != 35200 {
		t.Errorf("unexpected image fields: %+v", gotImg)
	}

	list, err := d.ListGeneratedImages(ctx, 10, 0)
	if err != nil {
		t.Fatalf("failed to list generated images: %v", err)
	}
	if len(list) != 1 || list[0].ID != "img_123456" {
		t.Errorf("unexpected image list: %+v", list)
	}

	if err := d.DeleteGeneratedImage(ctx, "img_123456"); err != nil {
		t.Fatalf("failed to delete image: %v", err)
	}
	listAfter, _ := d.ListGeneratedImages(ctx, 10, 0)
	if len(listAfter) != 0 {
		t.Errorf("expected 0 images after delete, got %d", len(listAfter))
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

func TestProfilePreserveReasoningAndCacheReuse(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	_ = d.SaveEngine(ctx, Engine{ID: "e1", Name: "E1", BinaryPath: "/bin/true"})
	_ = d.SaveModel(ctx, Model{ID: "m1", Name: "M1", EngineID: "e1", ModelPath: "/m.gguf"})

	harness := Profile{
		ModelID:           "m1",
		Name:              "harness",
		CtxSize:           8192,
		EnableUI:          true,
		Tools:             "safe",
		ReasoningFormat:   "deepseek",
		PreserveReasoning: true,
		CacheReuse:        256,
	}
	if err := d.SaveProfile(ctx, harness); err != nil {
		t.Fatalf("failed to save harness profile: %v", err)
	}

	got, err := d.GetProfile(ctx, "m1", "harness")
	if err != nil || got == nil {
		t.Fatalf("failed to get harness profile: %v", err)
	}
	if !got.PreserveReasoning {
		t.Errorf("expected preserve_reasoning=true roundtrip, got %+v", got)
	}
	if got.CacheReuse != 256 {
		t.Errorf("expected cache_reuse=256 roundtrip, got %d", got.CacheReuse)
	}
	if got.ReasoningFormat != "deepseek" {
		t.Errorf("expected reasoning_format=deepseek, got %q", got.ReasoningFormat)
	}

	plain := Profile{ModelID: "m1", Name: "plain", CtxSize: 8192, EnableUI: true, Tools: "safe"}
	if err := d.SaveProfile(ctx, plain); err != nil {
		t.Fatalf("failed to save plain profile: %v", err)
	}
	gotPlain, err := d.GetProfile(ctx, "m1", "plain")
	if err != nil || gotPlain == nil {
		t.Fatalf("failed to get plain profile: %v", err)
	}
	if gotPlain.PreserveReasoning {
		t.Errorf("expected preserve_reasoning=false by default, got %+v", gotPlain)
	}
	if gotPlain.CacheReuse != 0 {
		t.Errorf("expected cache_reuse=0 by default, got %d", gotPlain.CacheReuse)
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

func TestTrainingJobsCRUD(t *testing.T) {
	ctx := context.Background()
	d := setupTestDB(t)

	job := TrainingJob{
		ID:             "train-test-1",
		Name:           "LFM2.5 Classifier",
		BaseModelPath:  "/models/LFM2.5-2.6B",
		DatasetPath:    "/data/train.jsonl",
		ValDatasetPath: "/data/val.jsonl",
		OutputName:     "lfm2.5-uporka",
		Epochs:         3,
		BatchSize:      2,
		GradAccum:      8,
		LearningRate:   0.0002,
		LoraR:          16,
		LoraAlpha:      32,
		TargetModules:  "all-linear",
		QuantType:      "q8_0",
		Status:         "running",
	}

	if err := d.CreateTrainingJob(ctx, job); err != nil {
		t.Fatalf("failed to create training job: %v", err)
	}

	got, err := d.GetTrainingJob(ctx, "train-test-1")
	if err != nil || got == nil {
		t.Fatalf("failed to get training job: %v", err)
	}
	if got.Name != job.Name || got.BaseModelPath != job.BaseModelPath {
		t.Errorf("expected job %+v, got %+v", job, got)
	}

	// Update progress
	if err := d.UpdateTrainingJobProgress(ctx, "train-test-1", 10, 162, 0.18, 2.57, 2.45, 0.57); err != nil {
		t.Fatalf("failed to update progress: %v", err)
	}

	updated, err := d.GetTrainingJob(ctx, "train-test-1")
	if err != nil || updated == nil {
		t.Fatalf("failed to get updated job: %v", err)
	}
	if updated.CurrentStep != 10 || updated.CurrentLoss != 2.57 {
		t.Errorf("unexpected updated progress: %+v", updated)
	}

	// Complete job
	if err := d.UpdateTrainingJobStatus(ctx, "train-test-1", "completed", "/models/uporka/lfm.gguf", ""); err != nil {
		t.Fatalf("failed to complete job: %v", err)
	}

	completed, _ := d.GetTrainingJob(ctx, "train-test-1")
	if completed.Status != "completed" || completed.OutputGGUFPath != "/models/uporka/lfm.gguf" {
		t.Errorf("unexpected completed job: %+v", completed)
	}

	// List
	list, err := d.ListTrainingJobs(ctx, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 job in list, got %d (err: %v)", len(list), err)
	}

	// Delete
	if err := d.DeleteTrainingJob(ctx, "train-test-1"); err != nil {
		t.Fatalf("failed to delete job: %v", err)
	}
	deleted, _ := d.GetTrainingJob(ctx, "train-test-1")
	if deleted != nil {
		t.Errorf("expected job to be deleted, got %+v", deleted)
	}
}

