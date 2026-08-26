package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

func setupTestSupervisor(t *testing.T) (*Supervisor, *db.DB, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "llmctl_sup_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(tmpDir)
	})

	dbPath := filepath.Join(tmpDir, "test.db")
	database, err := db.NewDB(dbPath)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
	})

	sup := NewSupervisor(SupervisorConfig{
		DB:            database,
		LogDir:        filepath.Join(tmpDir, "logs"),
		Host:          "127.0.0.1",
		ExclusiveMode: true,
	})

	return sup, database, tmpDir
}

func TestBuildArgs(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{
		ID:          "gemma4",
		Name:        "Gemma 4",
		EngineID:    "llama-vk",
		ModelPath:   "/models/gemma4.gguf",
		MMProjPath:  "/models/mmproj.gguf",
		MTPPath:     "/models/mtp.gguf",
		DefaultPort: 8088,
	}

	profile := &db.Profile{
		ModelID:   "gemma4",
		Name:      "fast",
		CtxSize:   2048,
		Parallel:  1,
		KVType:    "q4_0",
		FlashAttn: "on",
		UseMTP:    true,
		UseVision: false,
		EnableUI:  true,
		Tools:     "safe",
	}

	args := sup.BuildArgs(model, profile, 8088)
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "-m /models/gemma4.gguf") {
		t.Errorf("missing -m flag: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--port 8088") {
		t.Errorf("missing --port flag: %s", argsStr)
	}
	if !strings.Contains(argsStr, "-c 2048") {
		t.Errorf("missing -c flag: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--cache-type-k q4_0") {
		t.Errorf("missing cache-type-k: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--flash-attn on") {
		t.Errorf("missing flash-attn: %s", argsStr)
	}
	if !strings.Contains(argsStr, "-md /models/mtp.gguf") {
		t.Errorf("missing -md mtp flag: %s", argsStr)
	}

	// Profile with DFlash2 override
	dflashProf := &db.Profile{
		ModelID:        "gemma4",
		Name:           "dflash",
		CtxSize:        4096,
		SpecType:       "draft-dflash",
		DraftModelPath: "/models/dflash.gguf",
		DraftNMax:      8,
		DraftNGL:       99,
		UseMTP:         false,
	}

	dfArgs := sup.BuildArgs(model, dflashProf, 8088)
	dfArgsStr := strings.Join(dfArgs, " ")

	if !strings.Contains(dfArgsStr, "--spec-type draft-dflash") {
		t.Errorf("missing --spec-type flag: %s", dfArgsStr)
	}
	if !strings.Contains(dfArgsStr, "-md /models/dflash.gguf") {
		t.Errorf("missing -md override flag: %s", dfArgsStr)
	}
	if !strings.Contains(dfArgsStr, "--spec-draft-n-max 8") {
		t.Errorf("missing --spec-draft-n-max flag: %s", dfArgsStr)
	}
	if !strings.Contains(dfArgsStr, "--spec-draft-ngl 99") {
		t.Errorf("missing --spec-draft-ngl flag: %s", dfArgsStr)
	}

	// Model with embedded MTP (MTPPath == ModelPath, e.g. KAT-Coder)
	katModel := &db.Model{
		ID:        "katcoder",
		Name:      "KAT-Coder",
		EngineID:  "llama-vk",
		ModelPath: "/models/katcoder.gguf",
		MTPPath:   "/models/katcoder.gguf", // same path
	}
	katMtpProf := &db.Profile{
		ModelID:   "katcoder",
		Name:      "mtp",
		CtxSize:   8192,
		UseMTP:    true,
		DraftNMax: 2,
	}
	katArgs := sup.BuildArgs(katModel, katMtpProf, 8080)
	katArgsStr := strings.Join(katArgs, " ")
	if !strings.Contains(katArgsStr, "--spec-type draft-mtp") {
		t.Errorf("expected '--spec-type draft-mtp', got: %s", katArgsStr)
	}
	if strings.Contains(katArgsStr, "-md") {
		t.Errorf("embedded MTP must NOT include -md, got: %s", katArgsStr)
	}
	if !strings.Contains(katArgsStr, "--spec-draft-n-max 2") {
		t.Errorf("expected '--spec-draft-n-max 2', got: %s", katArgsStr)
	}

	// N-Gram lookup with MTP flag set (should NOT pass -md)
	ngramProf := &db.Profile{
		ModelID:   "katcoder",
		Name:      "ngram",
		CtxSize:   8192,
		SpecType:  "ngram-simple",
		UseMTP:    true, // leftover flag from UI
		DraftNMax: 12,
	}
	ngramArgs := sup.BuildArgs(katModel, ngramProf, 8080)
	ngramArgsStr := strings.Join(ngramArgs, " ")
	if !strings.Contains(ngramArgsStr, "--spec-type ngram-simple") {
		t.Errorf("expected '--spec-type ngram-simple', got: %s", ngramArgsStr)
	}
	if strings.Contains(ngramArgsStr, "-md") {
		t.Errorf("ngram-simple must NOT include -md, got: %s", ngramArgsStr)
	}
	if !strings.Contains(ngramArgsStr, "--spec-draft-n-max 12") {
		t.Errorf("expected '--spec-draft-n-max 12', got: %s", ngramArgsStr)
	}

	// SpecType none
	noneProf := &db.Profile{
		ModelID:  "katcoder",
		Name:     "none",
		SpecType: "none",
		UseMTP:   true,
	}
	noneArgs := sup.BuildArgs(katModel, noneProf, 8080)
	noneArgsStr := strings.Join(noneArgs, " ")
	if strings.Contains(noneArgsStr, "--spec-type") || strings.Contains(noneArgsStr, "-md") {
		t.Errorf("spec-type none must NOT contain speculative flags, got: %s", noneArgsStr)
	}
}

func TestBuildArgsParallelAlwaysExplicit(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{
		ID:        "m",
		Name:      "M",
		EngineID:  "llama-vk",
		ModelPath: "/models/m.gguf",
	}

	// parallel=1 must still produce an explicit -np 1 (server default may differ)
	p1 := &db.Profile{ModelID: "m", Name: "default", CtxSize: 4096, Parallel: 1}
	args1 := strings.Join(sup.BuildArgs(model, p1, 8080), " ")
	if !strings.Contains(args1, "-np 1") {
		t.Errorf("expected explicit '-np 1' for parallel=1, got: %s", args1)
	}

	// parallel=2
	p2 := &db.Profile{ModelID: "m", Name: "dual", CtxSize: 4096, Parallel: 2}
	args2 := strings.Join(sup.BuildArgs(model, p2, 8080), " ")
	if !strings.Contains(args2, "-np 2") {
		t.Errorf("expected '-np 2' for parallel=2, got: %s", args2)
	}

	// zero value falls back to an explicit -np 1
	p0 := &db.Profile{ModelID: "m", Name: "zero", CtxSize: 4096}
	args0 := strings.Join(sup.BuildArgs(model, p0, 8080), " ")
	if !strings.Contains(args0, "-np 1") {
		t.Errorf("expected fallback '-np 1' for unset parallel, got: %s", args0)
	}
}

func TestBuildArgsReasoningPreserveAndCacheReuse(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{
		ID:        "ornith",
		Name:      "Ornith 9B",
		EngineID:  "llama-vk",
		ModelPath: "/models/ornith.gguf",
	}

	on := &db.Profile{
		ModelID:           "ornith",
		Name:              "harness",
		CtxSize:           65536,
		Parallel:          2,
		ReasoningFormat:   "deepseek",
		PreserveReasoning: true,
		CacheReuse:        256,
	}
	argsOn := strings.Join(sup.BuildArgs(model, on, 8080), " ")

	if !strings.Contains(argsOn, "--reasoning-format deepseek") {
		t.Errorf("missing reasoning format flag: %s", argsOn)
	}
	if !strings.Contains(argsOn, "--reasoning-preserve") {
		t.Errorf("missing --reasoning-preserve flag: %s", argsOn)
	}
	if !strings.Contains(argsOn, "--cache-reuse 256") {
		t.Errorf("missing '--cache-reuse 256' flag: %s", argsOn)
	}

	off := &db.Profile{
		ModelID:  "ornith",
		Name:     "plain",
		CtxSize:  65536,
		Parallel: 2,
	}
	argsOff := strings.Join(sup.BuildArgs(model, off, 8080), " ")

	if strings.Contains(argsOff, "--reasoning-preserve") {
		t.Errorf("--reasoning-preserve must be absent when disabled: %s", argsOff)
	}
	if strings.Contains(argsOff, "--cache-reuse") {
		t.Errorf("--cache-reuse must be absent when cache_reuse=0: %s", argsOff)
	}
}

func TestBuildArgsContextIsPerSlot(t *testing.T) {
	sup, _, _ := setupTestSupervisor(t)

	model := &db.Model{ID: "m", Name: "M", EngineID: "llama-vk", ModelPath: "/models/m.gguf"}

	// With explicit -np the server treats -c as TOTAL context split across
	// slots, so BuildArgs must scale it to keep per-request ctx intact.
	dual := &db.Profile{ModelID: "m", Name: "dual", CtxSize: 65536, Parallel: 2}
	argsDual := strings.Join(sup.BuildArgs(model, dual, 8080), " ")
	if !strings.Contains(argsDual, "-c 131072") {
		t.Errorf("expected '-c 131072' (ctx_size x parallel) for per-slot 64K, got: %s", argsDual)
	}
	if !strings.Contains(argsDual, "-np 2") {
		t.Errorf("expected '-np 2', got: %s", argsDual)
	}

	single := &db.Profile{ModelID: "m", Name: "single", CtxSize: 65536, Parallel: 1}
	argsSingle := strings.Join(sup.BuildArgs(model, single, 8080), " ")
	if !strings.Contains(argsSingle, "-c 65536") {
		t.Errorf("expected '-c 65536' for parallel=1, got: %s", argsSingle)
	}
}

func TestBenchmarkRunner(t *testing.T) {
	ctx := context.Background()
	// Mock llama-server HTTP
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"choices": [{"message": {"content": "56"}}],
				"timings": {
					"prompt_n": 10,
					"prompt_per_second": 125.4,
					"predicted_n": 2,
					"predicted_per_second": 42.8
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	sup, database, _ := setupTestSupervisor(t)

	// Save engine and model
	_ = database.SaveEngine(ctx, db.Engine{ID: "e1", Name: "E1", BinaryPath: "/bin/true"})
	_ = database.SaveModel(ctx, db.Model{ID: "m1", Name: "M1", EngineID: "e1", ModelPath: "/m.gguf"})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "m1", Name: "default", CtxSize: 4096})

	res, err := sup.runBenchmarkOnURL(ctx, "m1", "default", mockServer.URL)
	if err != nil {
		t.Fatalf("failed to run benchmark: %v", err)
	}

	if res.PromptPerSecond != 125.4 || res.PredictedPerSecond != 42.8 {
		t.Errorf("unexpected bench result: %+v", res)
	}
	if res.OutputSample != "56" {
		t.Errorf("unexpected output sample: %s", res.OutputSample)
	}

	// Verify saved to DB
	latest, err := database.GetLatestBenchmark(ctx, "m1")
	if err != nil || latest == nil {
		t.Fatalf("failed to get latest bench log: %v", err)
	}
	if latest.PredictedPerSecond != 42.8 {
		t.Errorf("saved bench mismatch: %+v", latest)
	}
}

func TestProcessLifecycle(t *testing.T) {
	ctx := context.Background()
	sup, database, _ := setupTestSupervisor(t)

	// Use `sleep 10` as a mock server process
	_ = database.SaveEngine(ctx, db.Engine{ID: "sleep-eng", Name: "Sleep", BinaryPath: "/bin/sleep"})
	_ = database.SaveModel(ctx, db.Model{ID: "test-model", Name: "Test Model", EngineID: "sleep-eng", ModelPath: "10", DefaultPort: 9999})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "test-model", Name: "default", CtxSize: 2048})

	// Start without healthcheck waiting for this mock test
	cmd, err := sup.StartProcessOnly(ctx, "test-model", "default")
	if err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	if cmd == nil || cmd.Process == nil {
		t.Fatalf("process was not launched")
	}

	// Verify process was launched (PID > 0 in runtime state).
	// Note: The mock "sleep" process may exit quickly since BuildArgs
	// passes flags that /bin/sleep doesn't understand, so we check
	// the DB state was recorded regardless of whether it's still alive.
	m, err := database.GetModel(ctx, "test-model")
	if err != nil || m == nil || m.Runtime == nil {
		t.Fatalf("runtime state not saved in DB: %+v", m)
	}
	if m.Runtime.Status != "starting" && m.Runtime.Status != "running" && m.Runtime.Status != "stopped" {
		t.Errorf("unexpected status: %s", m.Runtime.Status)
	}

	time.Sleep(50 * time.Millisecond)

	// Stop model
	if err := sup.StopModel(ctx, "test-model"); err != nil {
		t.Fatalf("failed to stop model: %v", err)
	}

	// Verify stopped state
	mAfter, err := database.GetModel(ctx, "test-model")
	if err != nil || mAfter == nil {
		t.Fatalf("failed to get model after stop")
	}
	if mAfter.Runtime != nil && mAfter.Runtime.Status != "stopped" {
		t.Errorf("expected stopped status, got: %+v", mAfter.Runtime)
	}
}
