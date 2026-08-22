package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

func (b *Bot) RenderMainMenu(ctx context.Context) (string, InlineKeyboardMarkup, error) {
	models, err := b.db.ListModels(ctx)
	if err != nil {
		return "", InlineKeyboardMarkup{}, err
	}

	sysStat := supervisor.GetSystemStatus()
	activeCount := 0
	for _, m := range models {
		if m.Runtime != nil && (m.Runtime.Status == "running" || m.Runtime.Status == "starting") {
			activeCount++
		}
	}

	var sb strings.Builder
	sb.WriteString("🖥 *LLM Control Center*\n\n")
	sb.WriteString(fmt.Sprintf("📊 *RAM:* %d / %d MB (%.0f%%)", sysStat.RAMUsedMB, sysStat.RAMTotalMB, sysStat.RAMUsagePct))
	if sysStat.VRAMTotalMB > 0 {
		sb.WriteString(fmt.Sprintf(" | *VRAM:* %d / %d MB", sysStat.VRAMUsedMB, sysStat.VRAMTotalMB))
	}
	sb.WriteString("\n")

	if activeCount > 0 {
		sb.WriteString(fmt.Sprintf("🟢 *Активно моделей:* %d\n", activeCount))
	} else {
		sb.WriteString("⚪ *Все модели остановлены (IDLE)*\n")
	}
	sb.WriteString("\nВыберите модель для управления или воспользуйтесь быстрым действием:")

	var rows [][]InlineKeyboardButton

	// Row for active/favorite models
	var modelButtons []InlineKeyboardButton
	for _, m := range models {
		icon := "🔴"
		if m.Runtime != nil {
			if m.Runtime.Status == "running" {
				icon = "🟢"
			} else if m.Runtime.Status == "starting" {
				icon = "🟡"
			}
		}
		btnText := fmt.Sprintf("%s %s", icon, m.ID)
		if m.IsFavorite {
			btnText += " ⭐"
		}
		modelButtons = append(modelButtons, InlineKeyboardButton{
			Text:         btnText,
			CallbackData: "nav:model:" + m.ID,
		})
		if len(modelButtons) == 2 {
			rows = append(rows, modelButtons)
			modelButtons = []InlineKeyboardButton{}
		}
	}
	if len(modelButtons) > 0 {
		rows = append(rows, modelButtons)
	}

	// Action controls
	rows = append(rows, []InlineKeyboardButton{
		{Text: "⏹ Остановить всё", CallbackData: "act:stopall"},
		{Text: "🔄 Обновить", CallbackData: "nav:main"},
	})
	rows = append(rows, []InlineKeyboardButton{
		{Text: "📊 Подробно о системе", CallbackData: "nav:sys"},
	})

	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (b *Bot) RenderModelCard(ctx context.Context, modelID string) (string, InlineKeyboardMarkup, error) {
	m, err := b.db.GetModel(ctx, modelID)
	if err != nil || m == nil {
		return "❌ Модель не найдена", InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{{Text: "⬅️ Главное меню", CallbackData: "nav:main"}},
			},
		}, nil
	}

	statusIcon := "🔴 Остановлена"
	pidStr := "-"
	activeProf := m.DefaultProfile
	if activeProf == "" {
		activeProf = "default"
	}

	if m.Runtime != nil {
		switch m.Runtime.Status {
		case "running":
			statusIcon = "🟢 Работает"
			pidStr = fmt.Sprintf("%d", m.Runtime.PID)
			activeProf = m.Runtime.ProfileName
		case "starting":
			statusIcon = "🟡 Запускается..."
			pidStr = fmt.Sprintf("%d", m.Runtime.PID)
			activeProf = m.Runtime.ProfileName
		}
	}

	bench, _ := b.db.GetLatestBenchmark(ctx, modelID)
	tpsStr := "нет данных"
	if bench != nil {
		tpsStr = fmt.Sprintf("%.1f tok/s (промпт: %.1f tok/s)", bench.PredictedPerSecond, bench.PromptPerSecond)
	}

	favIcon := "☆ Добавить в ⭐"
	if m.IsFavorite {
		favIcon = "⭐ Убрать из избранного"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🤖 *%s* (`%s`)\n\n", m.Name, m.ID))
	sb.WriteString(fmt.Sprintf("• *Статус:* %s\n", statusIcon))
	sb.WriteString(fmt.Sprintf("• *Порт:* `%d` | *PID:* `%s`\n", m.DefaultPort, pidStr))
	sb.WriteString(fmt.Sprintf("• *Активный профиль:* `%s`\n", activeProf))
	sb.WriteString(fmt.Sprintf("• *Скорость (тест):* %s\n", tpsStr))
	if m.MTPPath != "" {
		sb.WriteString("• *MTP спекулятор:* включен\n")
	}
	if m.MMProjPath != "" {
		sb.WriteString("• *Vision (mmproj):* поддерживается\n")
	}

	var rows [][]InlineKeyboardButton

	// Control buttons
	if m.Runtime != nil && m.Runtime.Status == "running" {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "⏹ Остановить", CallbackData: "act:stop:" + m.ID},
			{Text: "🧪 Benchmark", CallbackData: "act:bench:" + m.ID},
		})
	} else {
		rows = append(rows, []InlineKeyboardButton{
			{Text: "▶️ Старт (" + activeProf + ")", CallbackData: "act:start:" + m.ID + ":" + activeProf},
			{Text: "⚡ Профили...", CallbackData: "nav:profiles:" + m.ID},
		})
	}

	rows = append(rows, []InlineKeyboardButton{
		{Text: "📜 Логи", CallbackData: "nav:logs:" + m.ID},
		{Text: favIcon, CallbackData: "act:fav:" + m.ID},
	})
	rows = append(rows, []InlineKeyboardButton{
		{Text: "⬅️ Назад в меню", CallbackData: "nav:main"},
	})

	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (b *Bot) RenderProfilePicker(ctx context.Context, modelID string) (string, InlineKeyboardMarkup, error) {
	m, err := b.db.GetModel(ctx, modelID)
	if err != nil || m == nil {
		return "❌ Модель не найдена", InlineKeyboardMarkup{}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚡ *Выберите профиль для запуска %s:*\n\n", m.Name))
	for _, p := range m.Profiles {
		sb.WriteString(fmt.Sprintf("• *%s*: ctx %d", p.Name, p.CtxSize))
		if p.Description != "" {
			sb.WriteString(fmt.Sprintf(" — _%s_", p.Description))
		}
		sb.WriteString("\n")
	}

	var rows [][]InlineKeyboardButton
	var pBtns []InlineKeyboardButton
	for _, p := range m.Profiles {
		pBtns = append(pBtns, InlineKeyboardButton{
			Text:         "▶️ " + p.Name,
			CallbackData: fmt.Sprintf("act:start:%s:%s", m.ID, p.Name),
		})
		if len(pBtns) == 2 {
			rows = append(rows, pBtns)
			pBtns = []InlineKeyboardButton{}
		}
	}
	if len(pBtns) > 0 {
		rows = append(rows, pBtns)
	}

	rows = append(rows, []InlineKeyboardButton{
		{Text: "⬅️ Назад к модели", CallbackData: "nav:model:" + m.ID},
	})

	return sb.String(), InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (b *Bot) RenderLogs(ctx context.Context, modelID string) (string, InlineKeyboardMarkup, error) {
	logs, _ := b.sup.GetLogs(modelID, 20)
	if logs == "" {
		logs = "(лог-файл пока пуст)"
	}

	text := fmt.Sprintf("📜 *Логи модели %s (последние строки):*\n\n```\n%s\n```", modelID, logs)
	kb := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "🔄 Обновить логи", CallbackData: "nav:logs:" + modelID},
				{Text: "⬅️ Назад к модели", CallbackData: "nav:model:" + modelID},
			},
		},
	}
	return text, kb, nil
}

func (b *Bot) RenderSystemStats() (string, InlineKeyboardMarkup) {
	sysStat := supervisor.GetSystemStatus()
	var sb strings.Builder
	sb.WriteString("📊 *Информация о системе*\n\n")
	sb.WriteString(fmt.Sprintf("🧠 *RAM всего:* %d MB\n", sysStat.RAMTotalMB))
	sb.WriteString(fmt.Sprintf("🧠 *RAM занято:* %d MB (%.1f%%)\n", sysStat.RAMUsedMB, sysStat.RAMUsagePct))
	sb.WriteString(fmt.Sprintf("🧠 *RAM свободно:* %d MB\n\n", sysStat.RAMFreeMB))

	if sysStat.VRAMTotalMB > 0 {
		sb.WriteString(fmt.Sprintf("🎮 *Графика:* %s\n", sysStat.GPUName))
		sb.WriteString(fmt.Sprintf("🎮 *VRAM всего:* %d MB\n", sysStat.VRAMTotalMB))
		sb.WriteString(fmt.Sprintf("🎮 *VRAM занято:* %d MB (%.1f%%)\n", sysStat.VRAMUsedMB, sysStat.VRAMUsagePct))
		sb.WriteString(fmt.Sprintf("🎮 *VRAM свободно:* %d MB\n", sysStat.VRAMFreeMB))
	} else {
		sb.WriteString("🎮 *Графика:* VRAM не обнаружена (используется системная RAM / Vulkan)\n")
	}

	kb := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "🔄 Обновить", CallbackData: "nav:sys"},
				{Text: "⬅️ Главное меню", CallbackData: "nav:main"},
			},
		},
	}
	return sb.String(), kb
}
