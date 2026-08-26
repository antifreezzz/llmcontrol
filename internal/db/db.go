package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
	mu sync.RWMutex
}

func NewDB(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", dbPath)
	sqliteDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	d := &DB{db: sqliteDB}
	if err := d.initSchema(context.Background()); err != nil {
		sqliteDB.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	_ = d.ReconcileRuntimeStates(context.Background())

	return d, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) initSchema(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS engines (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		binary_path TEXT NOT NULL,
		default_args TEXT DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS models (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		engine_id TEXT NOT NULL REFERENCES engines(id),
		model_path TEXT NOT NULL,
		mmproj_path TEXT DEFAULT '',
		mtp_path TEXT DEFAULT '',
		default_port INTEGER DEFAULT 8088,
		default_profile TEXT DEFAULT 'default',
		is_favorite BOOLEAN DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		description TEXT DEFAULT '',
		ctx_size INTEGER NOT NULL DEFAULT 4096,
		parallel INTEGER DEFAULT 1,
		kv_type TEXT DEFAULT 'q8_0',
		flash_attn TEXT DEFAULT 'auto',
		use_mtp BOOLEAN DEFAULT 1,
		spec_type TEXT DEFAULT '',
		draft_model TEXT DEFAULT '',
		draft_n_max INTEGER DEFAULT 0,
		draft_ngl INTEGER DEFAULT 0,
		use_vision BOOLEAN DEFAULT 0,
		enable_ui BOOLEAN DEFAULT 1,
		allow_lan BOOLEAN DEFAULT 0,
		tools TEXT DEFAULT 'safe',
		extra_args TEXT DEFAULT '',
		UNIQUE(model_id, name)
	);

	CREATE TABLE IF NOT EXISTS runtime_state (
		model_id TEXT PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
		pid INTEGER DEFAULT 0,
		port INTEGER NOT NULL,
		host TEXT DEFAULT '127.0.0.1',
		profile_name TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at DATETIME,
		last_error TEXT DEFAULT ''
	);

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

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	if _, err := d.db.ExecContext(ctx, schema); err != nil {
		return err
	}

	// Auto-migrations for existing tables
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN spec_type TEXT DEFAULT ''")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN draft_model TEXT DEFAULT ''")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN draft_n_max INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN draft_ngl INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN allow_lan BOOLEAN DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN reasoning TEXT DEFAULT 'auto'")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN reasoning_format TEXT DEFAULT 'auto'")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN reasoning_budget INTEGER DEFAULT -1")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN preserve_reasoning BOOLEAN DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN cache_reuse INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE runtime_state ADD COLUMN host TEXT DEFAULT '127.0.0.1'")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET spec_type = '' WHERE spec_type IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_model = '' WHERE draft_model IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_n_max = 0 WHERE draft_n_max IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_ngl = 0 WHERE draft_ngl IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET allow_lan = 0 WHERE allow_lan IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning = 'auto' WHERE reasoning IS NULL OR reasoning = ''")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning_format = 'auto' WHERE reasoning_format IS NULL OR reasoning_format = ''")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning_budget = -1 WHERE reasoning_budget IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE runtime_state SET host = '127.0.0.1' WHERE host IS NULL OR host = ''")

	return nil
}

// Engines
func (d *DB) SaveEngine(ctx context.Context, e Engine) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	query := `
	INSERT INTO engines (id, name, binary_path, default_args)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name,
		binary_path=excluded.binary_path,
		default_args=excluded.default_args;
	`
	_, err := d.db.ExecContext(ctx, query, e.ID, e.Name, e.BinaryPath, e.DefaultArgs)
	return err
}

func (d *DB) GetEngine(ctx context.Context, id string) (*Engine, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, "SELECT id, name, binary_path, default_args FROM engines WHERE id = ?", id)
	var e Engine
	if err := row.Scan(&e.ID, &e.Name, &e.BinaryPath, &e.DefaultArgs); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

func (d *DB) ListEngines(ctx context.Context) ([]Engine, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.QueryContext(ctx, "SELECT id, name, binary_path, default_args FROM engines ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Engine
	for rows.Next() {
		var e Engine
		if err := rows.Scan(&e.ID, &e.Name, &e.BinaryPath, &e.DefaultArgs); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}

// Models
func (d *DB) SaveModel(ctx context.Context, m Model) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	query := `
	INSERT INTO models (id, name, engine_id, model_path, mmproj_path, mtp_path, default_port, default_profile, is_favorite)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name,
		engine_id=excluded.engine_id,
		model_path=excluded.model_path,
		mmproj_path=excluded.mmproj_path,
		mtp_path=excluded.mtp_path,
		default_port=excluded.default_port,
		default_profile=excluded.default_profile,
		is_favorite=excluded.is_favorite;
	`
	_, err := d.db.ExecContext(ctx, query, m.ID, m.Name, m.EngineID, m.ModelPath, m.MMProjPath, m.MTPPath, m.DefaultPort, m.DefaultProfile, m.IsFavorite)
	return err
}

func (d *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, name, engine_id, model_path, mmproj_path, mtp_path, default_port, default_profile, is_favorite, created_at
		FROM models WHERE id = ?
	`, id)

	var m Model
	if err := row.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	// Profiles
	pRows, err := d.db.QueryContext(ctx, `
		SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), extra_args
		FROM profiles WHERE model_id = ? ORDER BY id
	`, id)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()

	for pRows.Next() {
		var p Profile
		if err := pRows.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ExtraArgs); err != nil {
			return nil, err
		}
		m.Profiles = append(m.Profiles, p)
	}

	// Runtime state
	rtRow := d.db.QueryRowContext(ctx, `SELECT model_id, pid, port, COALESCE(host, '127.0.0.1'), profile_name, status, started_at, last_error FROM runtime_state WHERE model_id = ?`, id)
	var rt RuntimeState
	if err := rtRow.Scan(&rt.ModelID, &rt.PID, &rt.Port, &rt.Host, &rt.ProfileName, &rt.Status, &rt.StartedAt, &rt.LastError); err == nil {
		d.sanitizeRuntimeState(ctx, &rt)
		m.Runtime = &rt
	}

	return &m, nil
}

func (d *DB) ListModels(ctx context.Context) ([]Model, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, name, engine_id, model_path, mmproj_path, mtp_path, default_port, default_profile, is_favorite, created_at
		FROM models ORDER BY is_favorite DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}

	// Populate profiles and runtime states
	for i := range list {
		id := list[i].ID
		pRows, err := d.db.QueryContext(ctx, `
			SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), extra_args
			FROM profiles WHERE model_id = ? ORDER BY id
		`, id)
		if err == nil {
			for pRows.Next() {
				var p Profile
				if err := pRows.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ExtraArgs); err == nil {
					list[i].Profiles = append(list[i].Profiles, p)
				}
			}
			pRows.Close()
		}

		rtRow := d.db.QueryRowContext(ctx, `SELECT model_id, pid, port, COALESCE(host, '127.0.0.1'), profile_name, status, started_at, last_error FROM runtime_state WHERE model_id = ?`, id)
		var rt RuntimeState
		if err := rtRow.Scan(&rt.ModelID, &rt.PID, &rt.Port, &rt.Host, &rt.ProfileName, &rt.Status, &rt.StartedAt, &rt.LastError); err == nil {
			d.sanitizeRuntimeState(ctx, &rt)
			list[i].Runtime = &rt
		}
	}

	return list, nil
}

func (d *DB) DeleteModel(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "DELETE FROM models WHERE id = ?", id)
	return err
}

func (d *DB) SetFavorite(ctx context.Context, id string, isFavorite bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "UPDATE models SET is_favorite = ? WHERE id = ?", isFavorite, id)
	return err
}

func (d *DB) ListFavorites(ctx context.Context) ([]Model, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, name, engine_id, model_path, mmproj_path, mtp_path, default_port, default_profile, is_favorite, created_at
		FROM models WHERE is_favorite = 1 ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, nil
}

// Profiles
func (d *DB) SaveProfile(ctx context.Context, p Profile) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.Reasoning == "" {
		p.Reasoning = "auto"
	}
	if p.ReasoningFormat == "" {
		p.ReasoningFormat = "auto"
	}
	if p.ReasoningBudget == 0 {
		// keep 0 if explicitly set, default -1 if not set
	}
	query := `
	INSERT INTO profiles (model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, spec_type, draft_model, draft_n_max, draft_ngl, use_vision, enable_ui, allow_lan, tools, reasoning, reasoning_format, reasoning_budget, preserve_reasoning, cache_reuse, extra_args)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(model_id, name) DO UPDATE SET
		description=excluded.description,
		ctx_size=excluded.ctx_size,
		parallel=excluded.parallel,
		kv_type=excluded.kv_type,
		flash_attn=excluded.flash_attn,
		use_mtp=excluded.use_mtp,
		spec_type=excluded.spec_type,
		draft_model=excluded.draft_model,
		draft_n_max=excluded.draft_n_max,
		draft_ngl=excluded.draft_ngl,
		use_vision=excluded.use_vision,
		enable_ui=excluded.enable_ui,
		allow_lan=excluded.allow_lan,
		tools=excluded.tools,
		reasoning=excluded.reasoning,
		reasoning_format=excluded.reasoning_format,
		reasoning_budget=excluded.reasoning_budget,
		preserve_reasoning=excluded.preserve_reasoning,
		cache_reuse=excluded.cache_reuse,
		extra_args=excluded.extra_args;
	`
	_, err := d.db.ExecContext(ctx, query, p.ModelID, p.Name, p.Description, p.CtxSize, p.Parallel, p.KVType, p.FlashAttn, p.UseMTP, p.SpecType, p.DraftModelPath, p.DraftNMax, p.DraftNGL, p.UseVision, p.EnableUI, p.AllowLAN, p.Tools, p.Reasoning, p.ReasoningFormat, p.ReasoningBudget, p.PreserveReasoning, p.CacheReuse, p.ExtraArgs)
	return err
}

func (d *DB) GetProfile(ctx context.Context, modelID, profileName string) (*Profile, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), extra_args
		FROM profiles WHERE model_id = ? AND name = ?
	`, modelID, profileName)
	var p Profile
	if err := row.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ExtraArgs); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (d *DB) DeleteProfile(ctx context.Context, modelID, profileName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "DELETE FROM profiles WHERE model_id = ? AND name = ?", modelID, profileName)
	return err
}

// Runtime State
func (d *DB) SetRuntimeState(ctx context.Context, r RuntimeState) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if r.StartedAt == nil && (r.Status == "starting" || r.Status == "running") {
		r.StartedAt = &now
	}
	if r.Host == "" {
		r.Host = "127.0.0.1"
	}
	query := `
	INSERT INTO runtime_state (model_id, pid, port, host, profile_name, status, started_at, last_error)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(model_id) DO UPDATE SET
		pid=excluded.pid,
		port=excluded.port,
		host=excluded.host,
		profile_name=excluded.profile_name,
		status=excluded.status,
		started_at=excluded.started_at,
		last_error=excluded.last_error;
	`
	_, err := d.db.ExecContext(ctx, query, r.ModelID, r.PID, r.Port, r.Host, r.ProfileName, r.Status, r.StartedAt, r.LastError)
	return err
}

func (d *DB) ClearRuntimeState(ctx context.Context, modelID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "DELETE FROM runtime_state WHERE model_id = ?", modelID)
	return err
}

func (d *DB) GetActiveRuntimeStates(ctx context.Context) ([]RuntimeState, error) {
	var list []RuntimeState
	var deadIDs []string

	// Read phase under RLock
	d.mu.RLock()
	rows, err := d.db.QueryContext(ctx, `
		SELECT model_id, pid, port, profile_name, status, started_at, last_error
		FROM runtime_state WHERE status IN ('starting', 'running')
	`)
	if err != nil {
		d.mu.RUnlock()
		return nil, err
	}
	for rows.Next() {
		var r RuntimeState
		if err := rows.Scan(&r.ModelID, &r.PID, &r.Port, &r.ProfileName, &r.Status, &r.StartedAt, &r.LastError); err != nil {
			rows.Close()
			d.mu.RUnlock()
			return nil, err
		}
		if IsPIDAlive(r.PID) {
			list = append(list, r)
		} else {
			deadIDs = append(deadIDs, r.ModelID)
		}
	}
	rows.Close()
	d.mu.RUnlock()

	// Cleanup phase under write Lock — synchronous, no goroutine
	if len(deadIDs) > 0 {
		d.mu.Lock()
		for _, id := range deadIDs {
			_, _ = d.db.ExecContext(ctx, `UPDATE runtime_state SET status = 'stopped', pid = 0 WHERE model_id = ?`, id)
		}
		d.mu.Unlock()
	}

	return list, nil
}

func (d *DB) sanitizeRuntimeState(_ context.Context, rt *RuntimeState) {
	if rt == nil {
		return
	}
	if (rt.Status == "running" || rt.Status == "starting") && !IsPIDAlive(rt.PID) {
		rt.Status = "stopped"
		rt.PID = 0
		// DB cleanup is deferred to ReconcileRuntimeStates or GetActiveRuntimeStates
		// to avoid spawning uncoordinated goroutines from within a read lock.
	}
}

// ReconcileRuntimeStates cleans up any lingering 'running'/'starting' models whose processes are dead.
func (d *DB) ReconcileRuntimeStates(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.QueryContext(ctx, `SELECT model_id, pid FROM runtime_state WHERE status IN ('starting', 'running')`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var deadIDs []string
	for rows.Next() {
		var modelID string
		var pid int
		if err := rows.Scan(&modelID, &pid); err == nil {
			if !IsPIDAlive(pid) {
				deadIDs = append(deadIDs, modelID)
			}
		}
	}

	for _, id := range deadIDs {
		_, _ = d.db.ExecContext(ctx, `UPDATE runtime_state SET status = 'stopped', pid = 0 WHERE model_id = ?`, id)
	}
	return nil
}

// IsPIDAlive checks whether a process with the given PID is currently active.
func IsPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Check /proc/<pid> on Linux
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		return true
	}
	// Fallback using syscall signal 0
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errno, ok := err.(syscall.Errno); ok && errno == syscall.EPERM {
		return true
	}
	return false
}

// Benchmark Logs
func (d *DB) SaveBenchmarkLog(ctx context.Context, b BenchmarkLog) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	query := `
	INSERT INTO benchmark_logs (model_id, profile_name, prompt_tokens, prompt_per_second, predicted_tokens, predicted_per_second, success, output_sample)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := d.db.ExecContext(ctx, query, b.ModelID, b.ProfileName, b.PromptTokens, b.PromptPerSecond, b.PredictedTokens, b.PredictedPerSecond, b.Success, b.OutputSample)
	return err
}

func (d *DB) GetLatestBenchmark(ctx context.Context, modelID string) (*BenchmarkLog, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, model_id, profile_name, timestamp, prompt_tokens, prompt_per_second, predicted_tokens, predicted_per_second, success, output_sample
		FROM benchmark_logs WHERE model_id = ? ORDER BY id DESC LIMIT 1
	`, modelID)
	var b BenchmarkLog
	if err := row.Scan(&b.ID, &b.ModelID, &b.ProfileName, &b.Timestamp, &b.PromptTokens, &b.PromptPerSecond, &b.PredictedTokens, &b.PredictedPerSecond, &b.Success, &b.OutputSample); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// Settings
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value
	`, key, value)
	return err
}

func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key)
	var v string
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return v, nil
}
