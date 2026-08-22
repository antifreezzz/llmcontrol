# LLM Control Center (`llmcontrol`)

⚡ **LLM Control Center** — легковесный (~10–15 MB RAM) системный менеджер и CLI-утилита на Go для управления жизненным циклом локальных LLM (Vulkan / SYCL `llama-server`).

Объединяет управление моделями, профилями инференса, бенчмаркингом и мониторингом ресурсов в единый интерфейс:
- 📱 **Telegram Bot** (отказоустойчивое inline-меню, защита от зависаний кнопок, доступ только для владельца).
- 🌐 **Встроенный Web UI** (Single-page dark dashboard на `http://localhost:8666` без Node.js / npm).
- 🤖 **Встроенный MCP Server** (Model Context Protocol по stdio для управления моделями через Claude, Cursor, Antigravity).
- 🔌 **REST API & SSE** (легкая интеграция с внешними проектами, например настольными дисплеями ESP32).
- 💾 **Чистый SQLite** (без внешних зависимостей и CGO).

---

## Быстрый старт

### 1. Сборка
```bash
go build -o llmctl ./cmd/llmctl
```

### 2. Импорт существующих скриптов из `~/bin`
Если у вас уже есть bash-скрипты запуска моделей (`gemma4-start`, `cyber-start` и т.д.):
```bash
./llmctl import ~/bin
```

### 3. Просмотр списка моделей
```bash
./llmctl list
```

### 4. Управление моделями
```bash
# Запуск модели с профилем
./llmctl start gemma4 --profile fast

# Запуск быстрого теста скорости (tok/s)
./llmctl bench gemma4

# Просмотр логов
./llmctl logs gemma4 -n 50

# Остановка
./llmctl stop gemma4
./llmctl stop --all
```

### 5. Запуск фонового демона (Web UI + Telegram Bot + REST API)
```bash
./llmctl daemon
```
Веб-интерфейс будет доступен по адресу: **`http://localhost:8666`**

---

## Настройка Telegram-бота

Скопируйте `config.example.json` в `~/.llmcontrol/config.json`:
```json
{
  "http_host": "0.0.0.0",
  "http_port": 8666,
  "db_path": "~/.llmcontrol/llmcontrol.db",
  "log_dir": "~/.llmcontrol/logs",
  "exclusive_mode": true,
  "telegram_token": "ВАШ_ТОКЕН_БОТА",
  "telegram_admin_id": ВАШ_TELEGRAM_USER_ID
}
```

---

## Подключение как MCP-сервер в ИИ-ассистентах

В конфигурацию MCP (например, Claude Desktop или Cursor):
```json
{
  "mcpServers": {
    "llmcontrol": {
      "command": "/путь/к/llmctl",
      "args": ["mcp"]
    }
  }
}
```

---

## Интеграция с ESP32-дисплеем (REST API)

* `GET http://localhost:8666/api/status` — возвращает JSON с активной моделью, скоростью `tok/s` и нагрузкой памяти RAM / GPU.
* `POST http://localhost:8666/api/models/stop-all` — остановить всё по нажатию кнопки на тач-экране.
* `POST http://localhost:8666/api/models/{id}/start` — запуск избранной модели.
