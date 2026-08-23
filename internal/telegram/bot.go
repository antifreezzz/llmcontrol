package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

type BotConfig struct {
	Token   string
	AdminID int64
	DB      *db.DB
	Sup     *supervisor.Supervisor
}

type Bot struct {
	token      string
	adminID    int64
	db         *db.DB
	sup        *supervisor.Supervisor
	httpClient *http.Client
	baseURL    string
}

func NewBot(cfg BotConfig) *Bot {
	return &Bot{
		token:      cfg.Token,
		adminID:    cfg.AdminID,
		db:         cfg.DB,
		sup:        cfg.Sup,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    fmt.Sprintf("https://api.telegram.org/bot%s", cfg.Token),
	}
}

func (b *Bot) IsAuthorized(userID int64) bool {
	if b.adminID == 0 {
		return true
	}
	return userID == b.adminID
}

func (b *Bot) apiCall(method string, payload interface{}) ([]byte, error) {
	bBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/%s", b.baseURL, method)
	resp, err := b.httpClient.Post(url, "application/json", bytes.NewReader(bBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if !res.OK {
		return nil, fmt.Errorf("telegram API error (%s): %s", method, res.Description)
	}

	return res.Result, nil
}

func (b *Bot) AnswerCallback(callbackID string, text string) {
	payload := map[string]interface{}{
		"callback_query_id": callbackID,
	}
	if text != "" {
		payload["text"] = text
	}
	_, _ = b.apiCall("answerCallbackQuery", payload)
}

func (b *Bot) SendMessage(chatID int64, text string, kb *InlineKeyboardMarkup) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	_, err := b.apiCall("sendMessage", payload)
	if err != nil {
		log.Printf("[telegram] SendMessage error: %v", err)
	}
	return err
}

func (b *Bot) EditMessage(chatID int64, messageID int, text string, kb *InlineKeyboardMarkup) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}

	_, err := b.apiCall("editMessageText", payload)
	if err != nil {
		// Ignore "message is not modified"
		if strings.Contains(err.Error(), "message is not modified") {
			return nil
		}
		log.Printf("[telegram] EditMessage error: %v", err)
		return err
	}
	return nil
}

func (b *Bot) SetCommands() error {
	commands := []BotCommand{
		{Command: "menu", Description: "Главная панель управления моделями"},
		{Command: "status", Description: "Быстрый статус системы и активных моделей"},
		{Command: "stopall", Description: "Остановить все запущенные модели"},
	}
	_, err := b.apiCall("setMyCommands", map[string]interface{}{"commands": commands})
	return err
}

func (b *Bot) HandleUpdate(ctx context.Context, u Update) {
	// Handle message
	if u.Message != nil {
		if !b.IsAuthorized(u.Message.From.ID) {
			return
		}
		b.handleMessage(ctx, u.Message)
		return
	}

	// Handle callback query
	if u.CallbackQuery != nil {
		if !b.IsAuthorized(u.CallbackQuery.From.ID) {
			b.AnswerCallback(u.CallbackQuery.ID, "⛔ Доступ запрещен")
			return
		}
		b.handleCallback(ctx, u.CallbackQuery)
		return
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *Message) {
	text := strings.TrimSpace(msg.Text)
	switch text {
	case "/start", "/menu":
		txt, kb, _ := b.RenderMainMenu(ctx)
		_ = b.SendMessage(msg.Chat.ID, txt, &kb)
	case "/status":
		txt, kb := b.RenderSystemStats()
		_ = b.SendMessage(msg.Chat.ID, txt, &kb)
	case "/stopall":
		_ = b.sup.StopAll(ctx)
		txt, kb, _ := b.RenderMainMenu(ctx)
		_ = b.SendMessage(msg.Chat.ID, "⏹ Все модели остановлены!\n\n"+txt, &kb)
	default:
		txt, kb, _ := b.RenderMainMenu(ctx)
		_ = b.SendMessage(msg.Chat.ID, txt, &kb)
	}
}

func (b *Bot) handleCallback(ctx context.Context, cb *CallbackQuery) {
	b.AnswerCallback(cb.ID, "") // Instant acknowledge

	if cb.Message == nil {
		return
	}

	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	data := cb.Data

	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return
	}

	actionType := parts[0]
	actionTarget := parts[1]

	switch actionType {
	case "nav":
		switch actionTarget {
		case "main":
			txt, kb, _ := b.RenderMainMenu(ctx)
			_ = b.EditMessage(chatID, msgID, txt, &kb)
		case "model":
			if len(parts) >= 3 {
				modelID := parts[2]
				txt, kb, _ := b.RenderModelCard(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "profiles":
			if len(parts) >= 3 {
				modelID := parts[2]
				txt, kb, _ := b.RenderProfilePicker(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "logs":
			if len(parts) >= 3 {
				modelID := parts[2]
				txt, kb, _ := b.RenderLogs(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "sys":
			txt, kb := b.RenderSystemStats()
			_ = b.EditMessage(chatID, msgID, txt, &kb)
		}

	case "act":
		switch actionTarget {
		case "start":
			if len(parts) >= 4 {
				modelID := parts[2]
				profileName := parts[3]
				_ = b.sup.StartModel(ctx, modelID, profileName)
				// Render updated card
				time.Sleep(200 * time.Millisecond)
				txt, kb, _ := b.RenderModelCard(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "stop":
			if len(parts) >= 3 {
				modelID := parts[2]
				_ = b.sup.StopModel(ctx, modelID)
				time.Sleep(200 * time.Millisecond)
				txt, kb, _ := b.RenderModelCard(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "stopall":
			_ = b.sup.StopAll(ctx)
			time.Sleep(200 * time.Millisecond)
			txt, kb, _ := b.RenderMainMenu(ctx)
			_ = b.EditMessage(chatID, msgID, txt, &kb)
		case "fav":
			if len(parts) >= 3 {
				modelID := parts[2]
				m, _ := b.db.GetModel(ctx, modelID)
				if m != nil {
					_ = b.db.SetFavorite(ctx, modelID, !m.IsFavorite)
				}
				txt, kb, _ := b.RenderModelCard(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		case "bench":
			if len(parts) >= 3 {
				modelID := parts[2]
				b.AnswerCallback(cb.ID, "🧪 Запуск бенчмарка...")
				_, _ = b.sup.RunBenchmark(ctx, modelID)
				txt, kb, _ := b.RenderModelCard(ctx, modelID)
				_ = b.EditMessage(chatID, msgID, txt, &kb)
			}
		}
	}
}

func (b *Bot) StartPolling(ctx context.Context) {
	_ = b.SetCommands()
	offset := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		payload := map[string]interface{}{
			"offset":  offset,
			"timeout": 20,
		}

		resBytes, err := b.apiCall("getUpdates", payload)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		var updates []Update
		if err := json.Unmarshal(resBytes, &updates); err == nil {
			for _, u := range updates {
				if u.UpdateID >= offset {
					offset = u.UpdateID + 1
				}
				b.HandleUpdate(ctx, u)
			}
		}
	}
}
