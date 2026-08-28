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

