package telegram

import (
	"context"
	"fmt"
	"html"
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
	sb.WriteString("🖥 <b>LLM Control Center</b>\n\n")
	sb.WriteString(fmt.Sprintf("📊 <b>RAM:</b> %d / %d MB (%.0f%%)", sysStat.RAMUsedMB, sysStat.RAMTotalMB, sysStat.RAMUsagePct))
	if sysStat.VRAMTotalMB > 0 {
		sb.WriteString(fmt.Sprintf(" | <b>VRAM:</b> %d / %d MB", sysStat.VRAMUsedMB, sysStat.VRAMTotalMB))
	}
	sb.WriteString("\n")

	if activeCount > 0 {
		sb.WriteString(fmt.Sprintf("🟢 <b>Активно моделей:</b> %d\n", activeCount))
	} else {
		sb.WriteString("⚪ <b>Все модели остановлены (IDLE)</b>\n")
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
	sb.WriteString(fmt.Sprintf("🤖 <b>%s</b> (<code>%s</code>)\n\n", html.EscapeString(m.Name), html.EscapeString(m.ID)))
	sb.WriteString(fmt.Sprintf("• <b>Статус:</b> %s\n", statusIcon))
	sb.WriteString(fmt.Sprintf("• <b>Порт:</b> <code>%d</code> | <b>PID:</b> <code>%s</code>\n", m.DefaultPort, pidStr))
	sb.WriteString(fmt.Sprintf("• <b>Активный профиль:</b> <code>%s</code>\n", html.EscapeString(activeProf)))
	sb.WriteString(fmt.Sprintf("• <b>Скорость (тест):</b> %s\n", html.EscapeString(tpsStr)))
	if m.MTPPath != "" {
		sb.WriteString("• <b>MTP спекулятор:</b> включен\n")
	}
	if m.MMProjPath != "" {
		sb.WriteString("• <b>Vision (mmproj):</b> поддерживается\n")
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
	sb.WriteString(fmt.Sprintf("⚡ <b>Выберите профиль для запуска %s:</b>\n\n", html.EscapeString(m.Name)))
	for _, p := range m.Profiles {
		sb.WriteString(fmt.Sprintf("• <b>%s</b>: ctx %d", html.EscapeString(p.Name), p.CtxSize))
		if p.Description != "" {
			sb.WriteString(fmt.Sprintf(" — <i>%s</i>", html.EscapeString(p.Description)))
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

	text := fmt.Sprintf("📜 <b>Логи модели %s (последние строки):</b>\n\n<pre>%s</pre>", html.EscapeString(modelID), html.EscapeString(logs))
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
	sb.WriteString("📊 <b>Информация о системе</b>\n\n")
	sb.WriteString(fmt.Sprintf("🧠 <b>RAM всего:</b> %d MB\n", sysStat.RAMTotalMB))
	sb.WriteString(fmt.Sprintf("🧠 <b>RAM занято:</b> %d MB (%.1f%%)\n", sysStat.RAMUsedMB, sysStat.RAMUsagePct))
	sb.WriteString(fmt.Sprintf("🧠 <b>RAM свободно:</b> %d MB\n\n", sysStat.RAMFreeMB))

	if sysStat.VRAMTotalMB > 0 {
		sb.WriteString(fmt.Sprintf("🎮 <b>Графика:</b> %s\n", html.EscapeString(sysStat.GPUName)))
		sb.WriteString(fmt.Sprintf("🎮 <b>VRAM всего:</b> %d MB\n", sysStat.VRAMTotalMB))
		sb.WriteString(fmt.Sprintf("🎮 <b>VRAM занято:</b> %d MB (%.1f%%)\n", sysStat.VRAMUsedMB, sysStat.VRAMUsagePct))
		sb.WriteString(fmt.Sprintf("🎮 <b>VRAM свободно:</b> %d MB\n", sysStat.VRAMFreeMB))
	} else {
		sb.WriteString("🎮 <b>Графика:</b> VRAM не обнаружена (используется системная RAM / Vulkan)\n")
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
