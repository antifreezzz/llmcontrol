package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
	"github.com/antifreezzz/llmcontrol/internal/tunnel"
)

func setupTestAPI(t *testing.T) (*Server, *db.DB, *supervisor.Supervisor) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "llmctl_api_*")
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

	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		DB:            database,
		LogDir:        filepath.Join(tmpDir, "logs"),
		Host:          "127.0.0.1",
		ExclusiveMode: true,
	})

	srv := NewServer(database, sup, nil, nil)
	return srv, database, sup
}

func TestAPIStatusAndModels(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestAPI(t)

	// Seed DB
	_ = database.SaveEngine(ctx, db.Engine{ID: "e1", Name: "E1", BinaryPath: "/bin"})
	_ = database.SaveModel(ctx, db.Model{ID: "gemma4", Name: "Gemma 4", EngineID: "e1", ModelPath: "/m.gguf", DefaultPort: 8088})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "gemma4", Name: "fast", CtxSize: 2048})

	// 1. GET /api/status (idle)
	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var statusResp StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&statusResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if statusResp.Status != "idle" {
		t.Errorf("expected idle status, got %s", statusResp.Status)
	}

	// 2. GET /api/models
	req2 := httptest.NewRequest("GET", "/api/models", nil)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec2.Code)
	}

	var models []db.Model
	if err := json.NewDecoder(rec2.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gemma4" {
		t.Errorf("unexpected models list: %+v", models)
	}

	// 3. POST /api/models/gemma4/favorite
	req3 := httptest.NewRequest("POST", "/api/models/gemma4/favorite", strings.NewReader(`{"favorite": true}`))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec3.Code)
	}

	m, _ := database.GetModel(ctx, "gemma4")
	if !m.IsFavorite {
		t.Errorf("expected is_favorite true")
	}
}

func TestModelCRUDAndProfiles(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestAPI(t)

	_ = database.SaveEngine(ctx, db.Engine{ID: "llama-vk", Name: "Llama Vulkan", BinaryPath: "/bin"})

	// 1. POST /api/models (create new model)
	newModelJSON := `{
		"id": "new-model",
		"name": "New Model 7B",
		"engine_id": "llama-vk",
		"model_path": "/models/new.gguf",
		"default_port": 8099,
		"default_profile": "fast",
		"profiles": [
			{"name": "fast", "ctx_size": 2048, "kv_type": "q4_0"},
			{"name": "default", "ctx_size": 4096, "kv_type": "q8_0"}
		]
	}`
	req := httptest.NewRequest("POST", "/api/models", strings.NewReader(newModelJSON))
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	m, err := database.GetModel(ctx, "new-model")
	if err != nil || m == nil {
		t.Fatalf("failed to retrieve created model: %v", err)
	}
	if len(m.Profiles) != 2 {
		t.Errorf("expected 2 profiles, got %d", len(m.Profiles))
	}

	// Mark as favorite
	if err := database.SetFavorite(ctx, "new-model", true); err != nil {
		t.Fatalf("failed to set favorite: %v", err)
	}

	// 2. PUT /api/models/new-model (update model)
	updateJSON := `{
		"name": "Updated Model 7B v2",
		"engine_id": "llama-vk",
		"model_path": "/models/new_v2.gguf",
		"default_port": 8099,
		"default_profile": "default"
	}`
	req2 := httptest.NewRequest("PUT", "/api/models/new-model", strings.NewReader(updateJSON))
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on update, got %d", rec2.Code)
	}

	mUpdated, _ := database.GetModel(ctx, "new-model")
	if mUpdated.Name != "Updated Model 7B v2" {
		t.Errorf("model name was not updated: %s", mUpdated.Name)
	}
	if !mUpdated.IsFavorite {
		t.Errorf("expected is_favorite to remain true after update")
	}

	// 3. DELETE /api/models/new-model
	req3 := httptest.NewRequest("DELETE", "/api/models/new-model", nil)
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on delete, got %d", rec3.Code)
	}

	mDeleted, _ := database.GetModel(ctx, "new-model")
	if mDeleted != nil {
		t.Errorf("expected model to be deleted")
	}
}

func TestImageGenerationAPI(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestAPI(t)

	// Seed an image record in DB
	img := db.GeneratedImage{
		ID:             "img_test_123",
		Prompt:         "a cute robot",
		NegativePrompt: "bad",
		ModelID:        "sd-model",
		Width:          512,
		Height:         512,
		Steps:          20,
		CFGScale:       7.0,
		Seed:           42,
		DurationMs:     1200,
		FilePath:       "/tmp/test_img.png",
	}
	_ = database.SaveGeneratedImage(ctx, img)

	// 1. GET /api/images
	req := httptest.NewRequest("GET", "/api/images", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var images []db.GeneratedImage
	if err := json.NewDecoder(rec.Body).Decode(&images); err != nil {
		t.Fatalf("failed to decode images list: %v", err)
	}
	if len(images) != 1 || images[0].ID != "img_test_123" {
		t.Errorf("unexpected images list: %+v", images)
	}

	// 2. GET /api/images/img_test_123
	req2 := httptest.NewRequest("GET", "/api/images/img_test_123", nil)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec2.Code)
	}

	var gotImg db.GeneratedImage
	if err := json.NewDecoder(rec2.Body).Decode(&gotImg); err != nil {
		t.Fatalf("failed to decode image: %v", err)
	}
	if gotImg.Prompt != "a cute robot" {
		t.Errorf("unexpected image prompt: %s", gotImg.Prompt)
	}

	// 3. DELETE /api/images/img_test_123
	req3 := httptest.NewRequest("DELETE", "/api/images/img_test_123", nil)
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on delete, got %d", rec3.Code)
	}

	deletedImg, _ := database.GetGeneratedImage(ctx, "img_test_123")
	if deletedImg != nil {
		t.Errorf("expected image to be deleted from DB")
	}
}

func TestAPITrainingJobs(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestAPI(t)

	// Seed training job
	job := db.TrainingJob{
		ID:            "train-api-test",
		Name:          "Test LFM2.5 Fine-Tune",
		BaseModelPath: "/models/lfm",
		DatasetPath:   "/data/train.jsonl",
		OutputName:    "lfm-test",
		Epochs:        3,
		BatchSize:     2,
		Status:        "pending",
	}
	_ = database.CreateTrainingJob(ctx, job)

	// 1. GET /api/training/jobs
	req := httptest.NewRequest("GET", "/api/training/jobs", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var jobs []db.TrainingJob
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatalf("failed to decode jobs list: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "train-api-test" {
		t.Errorf("unexpected jobs list: %+v", jobs)
	}

	// 2. GET /api/training/jobs/train-api-test
	req2 := httptest.NewRequest("GET", "/api/training/jobs/train-api-test", nil)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec2.Code)
	}

	var gotJob db.TrainingJob
	if err := json.NewDecoder(rec2.Body).Decode(&gotJob); err != nil {
		t.Fatalf("failed to decode job: %v", err)
	}
	if gotJob.Name != "Test LFM2.5 Fine-Tune" {
		t.Errorf("unexpected job name: %s", gotJob.Name)
	}

	// 3. GET /api/training/active (idle)
	req3 := httptest.NewRequest("GET", "/api/training/active", nil)
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec3.Code)
	}

	var activeResp map[string]interface{}
	if err := json.NewDecoder(rec3.Body).Decode(&activeResp); err != nil {
		t.Fatalf("failed to decode active response: %v", err)
	}
	if activeResp["active"] != false {
		t.Errorf("expected active=false, got: %+v", activeResp)
	}
}

func TestAPIOpenAIModels(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestAPI(t)

	_ = database.SaveEngine(ctx, db.Engine{ID: "e1", Name: "E1"})
	_ = database.SaveModel(ctx, db.Model{ID: "gemma4", Name: "Gemma 4", EngineID: "e1"})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(res.Data) != 1 || res.Data[0].ID != "gemma4" {
		t.Fatalf("unexpected models list: %+v", res)
	}
}

func TestAPITunnelEndpoints(t *testing.T) {
	srv, _, _ := setupTestAPI(t)

	// Status with nil manager
	req := httptest.NewRequest("GET", "/api/tunnel/status", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Status with real manager
	tm := tunnel.NewClientManager("127.0.0.1:8666", "vps.example.com", 8443, 8666, "token123")
	srv.SetTunnelManager(tm)

	req2 := httptest.NewRequest("GET", "/api/tunnel/status", nil)
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	var st tunnel.Status
	if err := json.NewDecoder(rec2.Body).Decode(&st); err != nil {
		t.Fatalf("decode status failed: %v", err)
	}
	if st.VPSHost != "vps.example.com" || st.State != "disconnected" {
		t.Fatalf("unexpected status: %+v", st)
	}

	// Config update
	cfgBody := `{"vps_host":"new-vps.com","vps_tunnel_port":9443,"vps_token":"newtok","target_model_id":"lfm25"}`
	req3 := httptest.NewRequest("POST", "/api/tunnel/config", strings.NewReader(cfgBody))
	rec3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec3.Code)
	}
	if tm.GetStatus().VPSHost != "new-vps.com" || tm.GetStatus().VPSTunnelPort != 9443 || tm.GetStatus().TargetModelID != "lfm25" {
		t.Fatalf("update config failed: %+v", tm.GetStatus())
	}
}

func TestAPIOnDemandEndpoints(t *testing.T) {
	srv, _, _ := setupTestAPI(t)
	srv.SetOnDemandConfig(true, 300)

	// GET /api/ondemand
	req := httptest.NewRequest("GET", "/api/ondemand", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		WakeOnRequest      bool `json:"wake_on_request"`
		IdleTimeoutSeconds int  `json:"idle_timeout_seconds"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !resp.WakeOnRequest || resp.IdleTimeoutSeconds != 300 {
		t.Fatalf("unexpected ondemand config: %+v", resp)
	}

	// POST /api/ondemand
	updateBody := `{"wake_on_request":false,"idle_timeout_seconds":600}`
	req2 := httptest.NewRequest("POST", "/api/ondemand", strings.NewReader(updateBody))
	rec2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	if err := json.NewDecoder(rec2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.WakeOnRequest || resp.IdleTimeoutSeconds != 600 {
		t.Fatalf("update ondemand failed: %+v", resp)
	}
}




