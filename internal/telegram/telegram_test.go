package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

func setupTestBot(t *testing.T) (*Bot, *db.DB, *supervisor.Supervisor) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "llmctl_tg_*")
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

	bot := NewBot(BotConfig{
		Token:   "dummy-token",
		AdminID: 12345678,
		DB:      database,
		Sup:     sup,
	})

	return bot, database, sup
}

func TestRenderScreens(t *testing.T) {
	ctx := context.Background()
	bot, database, _ := setupTestBot(t)

	// Seed model
	_ = database.SaveEngine(ctx, db.Engine{ID: "e1", Name: "E1", BinaryPath: "/bin"})
	_ = database.SaveModel(ctx, db.Model{
		ID:          "gemma4",
		Name:        "Gemma 4 26B",
		EngineID:    "e1",
		ModelPath:   "/models/gemma.gguf",
		DefaultPort: 8088,
		IsFavorite:  true,
	})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "gemma4", Name: "fast", CtxSize: 2048})
	_ = database.SaveProfile(ctx, db.Profile{ModelID: "gemma4", Name: "default", CtxSize: 4096})

	// 1. Render Main Menu
	txt, kb, err := bot.RenderMainMenu(ctx)
	if err != nil {
		t.Fatalf("failed to render main menu: %v", err)
	}
	if !strings.Contains(txt, "LLM Control Center") {
		t.Errorf("main menu missing title: %s", txt)
	}
	if len(kb.InlineKeyboard) == 0 {
		t.Errorf("expected buttons in main menu keyboard")
	}

	// 2. Render Model Card
	mTxt, mKb, err := bot.RenderModelCard(ctx, "gemma4")
	if err != nil {
		t.Fatalf("failed to render model card: %v", err)
	}
	if !strings.Contains(mTxt, "Gemma 4 26B") {
		t.Errorf("model card missing model name: %s", mTxt)
	}
	if len(mKb.InlineKeyboard) == 0 {
		t.Errorf("expected buttons in model card")
	}

	// 3. Render Profiles
	pTxt, pKb, err := bot.RenderProfilePicker(ctx, "gemma4")
	if err != nil {
		t.Fatalf("failed to render profile picker: %v", err)
	}
	if !strings.Contains(pTxt, "Выберите профиль") {
		t.Errorf("profile picker missing header: %s", pTxt)
	}
	if len(pKb.InlineKeyboard) < 2 { // fast, default + back
		t.Errorf("expected profile buttons")
	}
}

func TestAuthMiddleware(t *testing.T) {
	bot, _, _ := setupTestBot(t)

	if !bot.IsAuthorized(12345678) {
		t.Errorf("admin user should be authorized")
	}
	if bot.IsAuthorized(99999999) {
		t.Errorf("unknown user should NOT be authorized")
	}
}
