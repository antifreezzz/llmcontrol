# Спецификация системы LLM Control Center (`llmcontrol`)

## 1. Назначение
`llmcontrol` — легковесный (~10–15 MB RAM) системный сервис и CLI-утилита на Go для управления жизненным циклом локальных LLM (Vulkan / SYCL `llama-server`), конфигурациями, профилями инференса, бенчмаркингом и интеграцией с внешними интерфейсами (Telegram-бот, Web UI, MCP для AI, REST API для ESP32-дисплея).

---

## 2. База данных SQLite (`~/.llmcontrol/llmcontrol.db`)

### 2.1 Схема таблиц

```sql
-- Движки запуска (llama-server Vulkan, SYCL и др.)
CREATE TABLE IF NOT EXISTS engines (
    id TEXT PRIMARY KEY,               -- 'llama-vk', 'llama-sycl'
    name TEXT NOT NULL,                -- 'Llama.cpp Vulkan'
    binary_path TEXT NOT NULL,         -- '~/llama.cpp/build-vk/bin/llama-server'
    default_args TEXT DEFAULT ''       -- Базовые флаги запуска
);

-- Модели
CREATE TABLE IF NOT EXISTS models (
    id TEXT PRIMARY KEY,               -- 'gemma4', 'cyber', 'ornith'
    name TEXT NOT NULL,                -- 'Gemma 4 26B A4B'
    engine_id TEXT NOT NULL REFERENCES engines(id),
    model_path TEXT NOT NULL,          -- Путь к основному .gguf
    mmproj_path TEXT DEFAULT '',       -- Путь к mmproj .gguf (vision)
    mtp_path TEXT DEFAULT '',          -- Путь к MTP/спекулятивному .gguf
    default_port INTEGER DEFAULT 8088,
    default_profile TEXT DEFAULT 'default',
    is_favorite BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Профили инференса
CREATE TABLE IF NOT EXISTS profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    name TEXT NOT NULL,                -- 'default', 'fast', 'long', 'xlong', 'max', 'api', 'vision'
    description TEXT DEFAULT '',
    ctx_size INTEGER NOT NULL DEFAULT 4096,
    parallel INTEGER DEFAULT 1,
    kv_type TEXT DEFAULT 'q8_0',       -- 'q8_0', 'q4_0', 'f16'
    flash_attn TEXT DEFAULT 'auto',    -- 'auto', 'on', 'off'
    use_mtp BOOLEAN DEFAULT 1,
    use_vision BOOLEAN DEFAULT 0,
    enable_ui BOOLEAN DEFAULT 1,
    tools TEXT DEFAULT 'safe',         -- 'safe', 'all', ''
    extra_args TEXT DEFAULT '',
    UNIQUE(model_id, name)
);

-- Состояние выполнения (Runtime State)
CREATE TABLE IF NOT EXISTS runtime_state (
    model_id TEXT PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
    pid INTEGER DEFAULT 0,
    port INTEGER NOT NULL,
    profile_name TEXT NOT NULL,
    status TEXT NOT NULL,              -- 'stopped', 'starting', 'running', 'error'
    started_at DATETIME,
    last_error TEXT DEFAULT ''
);

-- История запусков и бенчмарков
CREATE TABLE IF NOT EXISTS benchmark_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id TEXT NOT NULL,
    profile_name TEXT NOT NULL,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    prompt_tokens INTEGER,
    prompt_per_second REAL,
    predicted_tokens INTEGER,
    predicted_per_second REAL,
    success BOOLEAN DEFAULT 1,
    output_sample TEXT
);

-- Системные настройки
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

---

## 3. Process Supervisor

### 3.1 Логика запуска (`Start`)
1. **Проверка Exclusive Lock**: если включен режим `exclusive_mode = true` (по умолчанию для Intel Arc VRAM), все запущенные модели предварительно останавливаются.
2. **Формирование аргументов**:
   * `-m <model_path>`
   * `--port <port>`
   * `--host <host>` (по умолчанию `127.0.0.1`)
   * `-c <ctx_size>`
   * `--cache-type-k <kv_type> --cache-type-v <kv_type>`
   * `--flash-attn` (если `on` или `auto`)
   * `-np <parallel>`
   * Если `use_mtp` и задан `mtp_path` -> `-md <mtp_path>`
   * Если `use_vision` и задан `mmproj_path` -> `--mmproj <mmproj_path>`
   * Если `!enable_ui` -> `--no-webui`
3. **Запуск процесса**:
   * Перенаправление stdout/stderr в `~/.llmcontrol/logs/<model_id>.log`.
   * Сохранение PID и состояния `starting`.
4. **Healthcheck Loop**:
   * Опрос `http://<host>:<port>/health` или `http://<host>:<port>/v1/models` с таймаутом (до 60 сек).
   * При успехе: статус меняется на `running`, запускается быстрый benchmark (`7*8=`), результат сохраняется в `benchmark_logs`.

### 3.2 Логика остановки (`Stop`)
1. Отправка `SIGTERM` процессу по `pid`.
2. Ожидание завершения до 10 секунд (проверка через `kill -0` / `os.FindProcess`).
3. Если процесс не завершился -> отправка `SIGKILL`.
4. Очистка записи в `runtime_state` (статус `stopped`, `pid = 0`).

---

## 4. REST API

Базовый путь: `/api` (порт по умолчанию `8666`).

| Метод | Путь | Описание |
| :--- | :--- | :--- |
| `GET` | `/api/status` | Краткий статус для ESP32 / дашборда (`active_model`, `status`, `tps`, `port`, `ram_used`, `vram_used`) |
| `GET` | `/api/models` | Полный список всех моделей, их профилей и текущего runtime статуса |
| `GET` | `/api/models/:id` | Детальная информация по конкретной модели |
| `POST` | `/api/models/:id/start` | Запуск модели. Body: `{"profile": "fast", "exclusive": true}` |
| `POST` | `/api/models/:id/stop` | Остановка модели |
| `POST` | `/api/models/stop-all` | Остановка всех запущенных моделей |
| `POST` | `/api/models/:id/bench` | Запуск немедленного замера производительности |
| `GET` | `/api/models/:id/logs` | Последние N строк лога (query: `?lines=50`) |
| `GET` | `/api/events` | Server-Sent Events (SSE) поток изменений статусов и логов |
| `POST` | `/api/favorites/:id/toggle` | Переключение статуса «избранное» |

---

## 5. Telegram Bot

### 5.1 Безопасность
* Middleware сверяет `msg.From.ID == config.TelegramAdminID`. Неавторизованные запросы игнорируются.

### 5.2 Архитектура меню и экранов
1. **Главный экран (`/menu` или `/start`)**:
   * Текст: Статус системы (RAM/VRAM), список запущенных моделей (🟢) и избранных моделей.
   * Кнопки:
     * `[ 🟢 gemma4 (8088) ]` (нажатие ведет в карточку модели)
     * `[ ▶️ fast: gemma4 ]` (быстрый старт избранных в 1 тап)
     * `[ ⏹ Остановить всё ]` | `[ 🔄 Обновить ]`
     * `[ 📁 Все модели ]` | `[ 📊 GPU / RAM ]`
2. **Карточка модели (`ScreenModel(id)`)**:
   * Текст: Имя, модель, текущий профиль, tok/s последнего замера, PID, порт.
   * Кнопки:
     * `[ ▶️ Старт (default) ]` | `[ ⚡ Профили... ]`
     * `[ ⏹ Стоп ]` | `[ 🧪 Benchmark ]`
     * `[ 📜 Логи ]` | `[ ⭐ В избранное ]`
     * `[ ⬅️ Назад в меню ]`
3. **Выбор профиля (`ScreenProfiles(id)`)**:
   * Кнопки всех доступных профилей модели (`fast`, `default`, `long`, `xlong`, `max`, `api`, `vision`).
   * `[ ⬅️ Назад к модели ]`
4. **Просмотр логов (`ScreenLogs(id)`)**:
   * Вывод последних 20 строк.
   * `[ 🔄 Обновить логи ]` | `[ ⬅️ Назад ]`

### 5.3 Safe Update Engine
* На каждый `CallbackQuery` вызывается немедленный `bot.AnswerCallbackQuery(id)`.
* При редактировании сообщения (`EditMessageText`) перехватывается и подавляется ошибка Telegram `Bad Request: message is not modified`.

---

## 6. Model Context Protocol (MCP)

`llmctl mcp` предоставляет JSON-RPC 2.0 интерфейс через `stdio` (и опционально HTTP/SSE):

1. `llm_list_models()`: Список всех моделей, их профилей и статусов (running/stopped).
2. `llm_start_model(model_id: string, profile?: string)`: Запуск модели с профилем.
3. `llm_stop_model(model_id: string)`: Остановка модели.
4. `llm_stop_all()`: Остановка всех запущенных моделей.
5. `llm_get_status()`: Получение текущего состояния активных моделей и нагрузки памяти.
6. `llm_benchmark(model_id: string)`: Запуск тестового промпта и возвращение токенов в секунду.
7. `llm_get_logs(model_id: string, lines?: number)`: Чтение последних строк лога.

---

## 7. Legacy Parser / Importer (`~/bin`)

Импортер сканирует директорию `~/bin` на наличие скриптов:
* Извлекает имя модели из префикса `<name>-start`.
* Парсит переменные bash:
  * `LLAMA_BIN` -> Engine (Vulkan / SYCL)
  * `MODEL_DIR`, `MODEL` -> `model_path`
  * `MMPROJ` -> `mmproj_path`
  * `MTP` -> `mtp_path`
  * `PORT` -> `default_port`
  * `PROFILES[name]` и `PROFILE_DESCR[name]` -> создание записей в `profiles`.
* Сохраняет всё в SQLite без перезаписи существующих ручных настроек (режим upsert/safe insert).
