package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

func setupTestMCP(t *testing.T) (*Server, *db.DB, *supervisor.Supervisor) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "llmctl_mcp_*")
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

	srv := NewServer(database, sup)
	return srv, database, sup
}

func TestMCPListTools(t *testing.T) {
	srv, _, _ := setupTestMCP(t)
	tools := srv.ListTools()
	if len(tools) == 0 {
		t.Fatalf("expected tools list to be non-empty")
	}

	hasStart := false
	for _, tool := range tools {
		if tool.Name == "llm_start_model" {
			hasStart = true
			break
		}
	}
	if !hasStart {
		t.Errorf("expected llm_start_model tool in list")
	}
}

func TestMCPExecuteTool(t *testing.T) {
	ctx := context.Background()
	srv, database, _ := setupTestMCP(t)

	// Seed model
	_ = database.SaveEngine(ctx, db.Engine{ID: "e1", Name: "E1", BinaryPath: "/bin"})
	_ = database.SaveModel(ctx, db.Model{ID: "gemma4", Name: "Gemma 4", EngineID: "e1", ModelPath: "/m.gguf", DefaultPort: 8088})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "gemma4", Name: "fast", CtxSize: 2048})

	// Call llm_list_models
	resp, err := srv.CallTool(ctx, "llm_list_models", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("failed to call llm_list_models: %v", err)
	}
	if resp.IsError {
		t.Errorf("tool returned error: %s", resp.Content[0].Text)
	}

	// Call llm_get_status
	statResp, err := srv.CallTool(ctx, "llm_get_status", json.RawMessage(`{}`))
	if err != nil || statResp.IsError {
		t.Fatalf("failed to call llm_get_status: %v", err)
	}
}
