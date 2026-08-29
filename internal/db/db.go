package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
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
		model_type TEXT DEFAULT 'llm',
		model_path TEXT NOT NULL,
		mmproj_path TEXT DEFAULT '',
		mtp_path TEXT DEFAULT '',
		vae_path TEXT DEFAULT '',
		clip_path TEXT DEFAULT '',
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
		reasoning TEXT DEFAULT 'auto',
		reasoning_format TEXT DEFAULT 'auto',
		reasoning_budget INTEGER DEFAULT -1,
		preserve_reasoning BOOLEAN DEFAULT 0,
		cache_reuse INTEGER DEFAULT 0,
		clip_on_cpu BOOLEAN DEFAULT 0,
		vae_on_cpu BOOLEAN DEFAULT 0,
		offload_params BOOLEAN DEFAULT 0,
		width INTEGER DEFAULT 0,
		height INTEGER DEFAULT 0,
		steps INTEGER DEFAULT 0,
		cfg_scale REAL DEFAULT 0.0,
		sampling_method TEXT DEFAULT '',
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

	CREATE TABLE IF NOT EXISTS generated_images (
		id TEXT PRIMARY KEY,
		prompt TEXT NOT NULL,
		negative_prompt TEXT DEFAULT '',
		model_id TEXT NOT NULL,
		profile_name TEXT DEFAULT '',
		width INTEGER DEFAULT 512,
		height INTEGER DEFAULT 512,
		steps INTEGER DEFAULT 20,
		cfg_scale REAL DEFAULT 7.0,
		seed INTEGER DEFAULT 0,
		duration_ms INTEGER DEFAULT 0,
		file_path TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS training_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		base_model_path TEXT NOT NULL,
		dataset_path TEXT NOT NULL,
		val_dataset_path TEXT DEFAULT '',
		output_name TEXT NOT NULL,
		epochs INTEGER DEFAULT 3,
		batch_size INTEGER DEFAULT 2,
		grad_accum INTEGER DEFAULT 8,
		learning_rate REAL DEFAULT 0.0002,
		lora_r INTEGER DEFAULT 16,
		lora_alpha INTEGER DEFAULT 32,
		target_modules TEXT DEFAULT 'all-linear',
		quant_type TEXT DEFAULT 'q8_0',
		status TEXT NOT NULL,
		current_step INTEGER DEFAULT 0,
		total_steps INTEGER DEFAULT 0,
		current_epoch REAL DEFAULT 0,
		current_loss REAL DEFAULT 0,
		val_loss REAL DEFAULT 0,
		accuracy REAL DEFAULT 0,
		error_message TEXT DEFAULT '',
		output_gguf_path TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		started_at DATETIME,
		completed_at DATETIME
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
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE models ADD COLUMN model_type TEXT DEFAULT 'llm'")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE models ADD COLUMN vae_path TEXT DEFAULT ''")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE models ADD COLUMN clip_path TEXT DEFAULT ''")
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
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN clip_on_cpu BOOLEAN DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN vae_on_cpu BOOLEAN DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN offload_params BOOLEAN DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN width INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN height INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN steps INTEGER DEFAULT 0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN cfg_scale REAL DEFAULT 0.0")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE profiles ADD COLUMN sampling_method TEXT DEFAULT ''")
	_, _ = d.db.ExecContext(ctx, "ALTER TABLE runtime_state ADD COLUMN host TEXT DEFAULT '127.0.0.1'")
	_, _ = d.db.ExecContext(ctx, "UPDATE models SET model_type = 'llm' WHERE model_type IS NULL OR model_type = ''")
	_, _ = d.db.ExecContext(ctx, "UPDATE models SET vae_path = '' WHERE vae_path IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE models SET clip_path = '' WHERE clip_path IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET spec_type = '' WHERE spec_type IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_model = '' WHERE draft_model IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_n_max = 0 WHERE draft_n_max IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET draft_ngl = 0 WHERE draft_ngl IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET allow_lan = 0 WHERE allow_lan IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning = 'auto' WHERE reasoning IS NULL OR reasoning = ''")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning_format = 'auto' WHERE reasoning_format IS NULL OR reasoning_format = ''")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET reasoning_budget = -1 WHERE reasoning_budget IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET clip_on_cpu = 0 WHERE clip_on_cpu IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET vae_on_cpu = 0 WHERE vae_on_cpu IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET offload_params = 0 WHERE offload_params IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET width = 0 WHERE width IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET height = 0 WHERE height IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET steps = 0 WHERE steps IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET cfg_scale = 0.0 WHERE cfg_scale IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE profiles SET sampling_method = '' WHERE sampling_method IS NULL")
	_, _ = d.db.ExecContext(ctx, "UPDATE runtime_state SET host = '127.0.0.1' WHERE host IS NULL OR host = ''")

	// Seed standard engines if available
	home, _ := os.UserHomeDir()
	vkBin := filepath.Join(home, "llama.cpp", "build-vk", "bin", "llama-server")
	if _, err := os.Stat(vkBin); err == nil {
		_ = d.SaveEngine(ctx, Engine{
			ID:          "llama-vk",
			Name:        "Llama.cpp Vulkan (Intel Arc / Vulkan)",
			BinaryPath:  vkBin,
			DefaultArgs: "",
		})
	}
	syclBin := filepath.Join(home, "llama-sycl", "llama-latest", "llama-server")
	if _, err := os.Stat(syclBin); err == nil {
		_ = d.SaveEngine(ctx, Engine{
			ID:          "llama-sycl",
			Name:        "Llama.cpp Intel SYCL (Level Zero / XMX)",
			BinaryPath:  syclBin,
			DefaultArgs: "",
		})
	}
	cpuBin, err := exec.LookPath("llama-server")
	if err == nil && cpuBin != vkBin && cpuBin != syclBin {
		_ = d.SaveEngine(ctx, Engine{
			ID:          "llama-cpu",
			Name:        "Llama.cpp CPU (Host AVX / Zen4)",
			BinaryPath:  cpuBin,
			DefaultArgs: "--ngl 0",
		})
	} else if err == nil {
		_ = d.SaveEngine(ctx, Engine{
			ID:          "llama-cpu",
			Name:        "Llama.cpp CPU (Host AVX / Zen4)",
			BinaryPath:  cpuBin,
			DefaultArgs: "--ngl 0",
		})
	}

	// Seed sd-vk engine if available
	sdVkPath := filepath.Join(home, ".unsloth", "vulkan-sd-cpp", "sd-cli")
	if _, err := os.Stat(sdVkPath); err == nil {
		_ = d.SaveEngine(ctx, Engine{
			ID:          "sd-vk",
			Name:        "Stable Diffusion Vulkan (sd-cli)",
			BinaryPath:  sdVkPath,
			DefaultArgs: "--mode img_gen",
		})
	}

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
	if m.ModelType == "" {
		m.ModelType = "llm"
	}
	query := `
	INSERT INTO models (id, name, engine_id, model_type, model_path, mmproj_path, mtp_path, vae_path, clip_path, default_port, default_profile, is_favorite)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name,
		engine_id=excluded.engine_id,
		model_type=excluded.model_type,
		model_path=excluded.model_path,
		mmproj_path=excluded.mmproj_path,
		mtp_path=excluded.mtp_path,
		vae_path=excluded.vae_path,
		clip_path=excluded.clip_path,
		default_port=excluded.default_port,
		default_profile=excluded.default_profile,
		is_favorite=excluded.is_favorite;
	`
	_, err := d.db.ExecContext(ctx, query, m.ID, m.Name, m.EngineID, m.ModelType, m.ModelPath, m.MMProjPath, m.MTPPath, m.VAEPath, m.CLIPPath, m.DefaultPort, m.DefaultProfile, m.IsFavorite)
	return err
}

func (d *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, name, engine_id, COALESCE(model_type, 'llm'), model_path, mmproj_path, mtp_path, COALESCE(vae_path, ''), COALESCE(clip_path, ''), default_port, default_profile, is_favorite, created_at
		FROM models WHERE id = ?
	`, id)

	var m Model
	if err := row.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelType, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.VAEPath, &m.CLIPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	// Profiles
	pRows, err := d.db.QueryContext(ctx, `
		SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), COALESCE(clip_on_cpu, 0), COALESCE(vae_on_cpu, 0), COALESCE(offload_params, 0), COALESCE(width, 0), COALESCE(height, 0), COALESCE(steps, 0), COALESCE(cfg_scale, 0.0), COALESCE(sampling_method, ''), extra_args
		FROM profiles WHERE model_id = ? ORDER BY id
	`, id)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()

	for pRows.Next() {
		var p Profile
		if err := pRows.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ClipOnCPU, &p.VAEOnCPU, &p.OffloadParams, &p.Width, &p.Height, &p.Steps, &p.CFGScale, &p.SamplingMethod, &p.ExtraArgs); err != nil {
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
		SELECT id, name, engine_id, COALESCE(model_type, 'llm'), model_path, mmproj_path, mtp_path, COALESCE(vae_path, ''), COALESCE(clip_path, ''), default_port, default_profile, is_favorite, created_at
		FROM models ORDER BY is_favorite DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelType, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.VAEPath, &m.CLIPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}

	// Populate profiles and runtime states
	for i := range list {
		id := list[i].ID
		pRows, err := d.db.QueryContext(ctx, `
			SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), COALESCE(clip_on_cpu, 0), COALESCE(vae_on_cpu, 0), COALESCE(offload_params, 0), COALESCE(width, 0), COALESCE(height, 0), COALESCE(steps, 0), COALESCE(cfg_scale, 0.0), COALESCE(sampling_method, ''), extra_args
			FROM profiles WHERE model_id = ? ORDER BY id
		`, id)
		if err == nil {
			for pRows.Next() {
				var p Profile
				if err := pRows.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ClipOnCPU, &p.VAEOnCPU, &p.OffloadParams, &p.Width, &p.Height, &p.Steps, &p.CFGScale, &p.SamplingMethod, &p.ExtraArgs); err == nil {
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
		SELECT id, name, engine_id, COALESCE(model_type, 'llm'), model_path, mmproj_path, mtp_path, COALESCE(vae_path, ''), COALESCE(clip_path, ''), default_port, default_profile, is_favorite, created_at
		FROM models WHERE is_favorite = 1 ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Name, &m.EngineID, &m.ModelType, &m.ModelPath, &m.MMProjPath, &m.MTPPath, &m.VAEPath, &m.CLIPPath, &m.DefaultPort, &m.DefaultProfile, &m.IsFavorite, &m.CreatedAt); err != nil {
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
	INSERT INTO profiles (model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, spec_type, draft_model, draft_n_max, draft_ngl, use_vision, enable_ui, allow_lan, tools, reasoning, reasoning_format, reasoning_budget, preserve_reasoning, cache_reuse, clip_on_cpu, vae_on_cpu, offload_params, width, height, steps, cfg_scale, sampling_method, extra_args)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		clip_on_cpu=excluded.clip_on_cpu,
		vae_on_cpu=excluded.vae_on_cpu,
		offload_params=excluded.offload_params,
		width=excluded.width,
		height=excluded.height,
		steps=excluded.steps,
		cfg_scale=excluded.cfg_scale,
		sampling_method=excluded.sampling_method,
		extra_args=excluded.extra_args;
	`
	_, err := d.db.ExecContext(ctx, query, p.ModelID, p.Name, p.Description, p.CtxSize, p.Parallel, p.KVType, p.FlashAttn, p.UseMTP, p.SpecType, p.DraftModelPath, p.DraftNMax, p.DraftNGL, p.UseVision, p.EnableUI, p.AllowLAN, p.Tools, p.Reasoning, p.ReasoningFormat, p.ReasoningBudget, p.PreserveReasoning, p.CacheReuse, p.ClipOnCPU, p.VAEOnCPU, p.OffloadParams, p.Width, p.Height, p.Steps, p.CFGScale, p.SamplingMethod, p.ExtraArgs)
	return err
}

func (d *DB) GetProfile(ctx context.Context, modelID, profileName string) (*Profile, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, model_id, name, description, ctx_size, parallel, kv_type, flash_attn, use_mtp, COALESCE(spec_type, ''), COALESCE(draft_model, ''), COALESCE(draft_n_max, 0), COALESCE(draft_ngl, 0), use_vision, enable_ui, COALESCE(allow_lan, 0), tools, COALESCE(reasoning, 'auto'), COALESCE(reasoning_format, 'auto'), COALESCE(reasoning_budget, -1), COALESCE(preserve_reasoning, 0), COALESCE(cache_reuse, 0), COALESCE(clip_on_cpu, 0), COALESCE(vae_on_cpu, 0), COALESCE(offload_params, 0), COALESCE(width, 0), COALESCE(height, 0), COALESCE(steps, 0), COALESCE(cfg_scale, 0.0), COALESCE(sampling_method, ''), extra_args
		FROM profiles WHERE model_id = ? AND name = ?
	`, modelID, profileName)
	var p Profile
	if err := row.Scan(&p.ID, &p.ModelID, &p.Name, &p.Description, &p.CtxSize, &p.Parallel, &p.KVType, &p.FlashAttn, &p.UseMTP, &p.SpecType, &p.DraftModelPath, &p.DraftNMax, &p.DraftNGL, &p.UseVision, &p.EnableUI, &p.AllowLAN, &p.Tools, &p.Reasoning, &p.ReasoningFormat, &p.ReasoningBudget, &p.PreserveReasoning, &p.CacheReuse, &p.ClipOnCPU, &p.VAEOnCPU, &p.OffloadParams, &p.Width, &p.Height, &p.Steps, &p.CFGScale, &p.SamplingMethod, &p.ExtraArgs); err != nil {
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

// Generated Images
func (d *DB) SaveGeneratedImage(ctx context.Context, img GeneratedImage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	query := `
	INSERT INTO generated_images (id, prompt, negative_prompt, model_id, profile_name, width, height, steps, cfg_scale, seed, duration_ms, file_path, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		prompt=excluded.prompt,
		negative_prompt=excluded.negative_prompt,
		model_id=excluded.model_id,
		profile_name=excluded.profile_name,
		width=excluded.width,
		height=excluded.height,
		steps=excluded.steps,
		cfg_scale=excluded.cfg_scale,
		seed=excluded.seed,
		duration_ms=excluded.duration_ms,
		file_path=excluded.file_path,
		created_at=excluded.created_at;
	`
	now := img.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	_, err := d.db.ExecContext(ctx, query, img.ID, img.Prompt, img.NegativePrompt, img.ModelID, img.ProfileName, img.Width, img.Height, img.Steps, img.CFGScale, img.Seed, img.DurationMs, img.FilePath, now)
	return err
}

func (d *DB) GetGeneratedImage(ctx context.Context, id string) (*GeneratedImage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, prompt, COALESCE(negative_prompt, ''), model_id, COALESCE(profile_name, ''), width, height, steps, cfg_scale, seed, duration_ms, file_path, created_at
		FROM generated_images WHERE id = ?
	`, id)
	var img GeneratedImage
	if err := row.Scan(&img.ID, &img.Prompt, &img.NegativePrompt, &img.ModelID, &img.ProfileName, &img.Width, &img.Height, &img.Steps, &img.CFGScale, &img.Seed, &img.DurationMs, &img.FilePath, &img.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &img, nil
}

func (d *DB) ListGeneratedImages(ctx context.Context, limit, offset int) ([]GeneratedImage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, prompt, COALESCE(negative_prompt, ''), model_id, COALESCE(profile_name, ''), width, height, steps, cfg_scale, seed, duration_ms, file_path, created_at
		FROM generated_images ORDER BY created_at DESC LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []GeneratedImage
	for rows.Next() {
		var img GeneratedImage
		if err := rows.Scan(&img.ID, &img.Prompt, &img.NegativePrompt, &img.ModelID, &img.ProfileName, &img.Width, &img.Height, &img.Steps, &img.CFGScale, &img.Seed, &img.DurationMs, &img.FilePath, &img.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, img)
	}
	return list, rows.Err()
}

func (d *DB) DeleteGeneratedImage(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "DELETE FROM generated_images WHERE id = ?", id)
	return err
}

func (d *DB) CreateTrainingJob(ctx context.Context, job TrainingJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO training_jobs (
			id, name, base_model_path, dataset_path, val_dataset_path, output_name,
			epochs, batch_size, grad_accum, learning_rate, lora_r, lora_alpha,
			target_modules, quant_type, status, current_step, total_steps,
			current_epoch, current_loss, val_loss, accuracy, error_message, output_gguf_path,
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		job.ID, job.Name, job.BaseModelPath, job.DatasetPath, job.ValDatasetPath, job.OutputName,
		job.Epochs, job.BatchSize, job.GradAccum, job.LearningRate, job.LoraR, job.LoraAlpha,
		job.TargetModules, job.QuantType, job.Status, job.CurrentStep, job.TotalSteps,
		job.CurrentEpoch, job.CurrentLoss, job.ValLoss, job.Accuracy, job.ErrorMessage, job.OutputGGUFPath,
		job.CreatedAt, job.StartedAt, job.CompletedAt,
	)
	return err
}

func (d *DB) GetTrainingJob(ctx context.Context, id string) (*TrainingJob, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	row := d.db.QueryRowContext(ctx, `
		SELECT id, name, base_model_path, dataset_path, COALESCE(val_dataset_path, ''), output_name,
		       epochs, batch_size, grad_accum, learning_rate, lora_r, lora_alpha,
		       target_modules, quant_type, status, current_step, total_steps,
		       current_epoch, current_loss, val_loss, accuracy,
		       COALESCE(error_message, ''), COALESCE(output_gguf_path, ''),
		       created_at, started_at, completed_at
		FROM training_jobs WHERE id = ?
	`, id)
	var j TrainingJob
	var startedAt, completedAt sql.NullTime
	if err := row.Scan(
		&j.ID, &j.Name, &j.BaseModelPath, &j.DatasetPath, &j.ValDatasetPath, &j.OutputName,
		&j.Epochs, &j.BatchSize, &j.GradAccum, &j.LearningRate, &j.LoraR, &j.LoraAlpha,
		&j.TargetModules, &j.QuantType, &j.Status, &j.CurrentStep, &j.TotalSteps,
		&j.CurrentEpoch, &j.CurrentLoss, &j.ValLoss, &j.Accuracy,
		&j.ErrorMessage, &j.OutputGGUFPath,
		&j.CreatedAt, &startedAt, &completedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}
	return &j, nil
}

func (d *DB) ListTrainingJobs(ctx context.Context, limit, offset int) ([]TrainingJob, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, name, base_model_path, dataset_path, COALESCE(val_dataset_path, ''), output_name,
		       epochs, batch_size, grad_accum, learning_rate, lora_r, lora_alpha,
		       target_modules, quant_type, status, current_step, total_steps,
		       current_epoch, current_loss, val_loss, accuracy,
		       COALESCE(error_message, ''), COALESCE(output_gguf_path, ''),
		       created_at, started_at, completed_at
		FROM training_jobs ORDER BY created_at DESC LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []TrainingJob
	for rows.Next() {
		var j TrainingJob
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(
			&j.ID, &j.Name, &j.BaseModelPath, &j.DatasetPath, &j.ValDatasetPath, &j.OutputName,
			&j.Epochs, &j.BatchSize, &j.GradAccum, &j.LearningRate, &j.LoraR, &j.LoraAlpha,
			&j.TargetModules, &j.QuantType, &j.Status, &j.CurrentStep, &j.TotalSteps,
			&j.CurrentEpoch, &j.CurrentLoss, &j.ValLoss, &j.Accuracy,
			&j.ErrorMessage, &j.OutputGGUFPath,
			&j.CreatedAt, &startedAt, &completedAt,
		); err != nil {
			return nil, err
		}
		if startedAt.Valid {
			j.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		list = append(list, j)
	}
	return list, rows.Err()
}

func (d *DB) UpdateTrainingJobProgress(ctx context.Context, id string, step, totalSteps int, epoch, loss, valLoss, acc float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, `
		UPDATE training_jobs
		SET current_step = ?, total_steps = ?, current_epoch = ?, current_loss = ?, val_loss = ?, accuracy = ?
		WHERE id = ?
	`, step, totalSteps, epoch, loss, valLoss, acc, id)
	return err
}

func (d *DB) UpdateTrainingJobStatus(ctx context.Context, id string, status string, outputGGUF string, errMsg string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	var startedAt, completedAt *time.Time
	if status == "running" {
		startedAt = &now
		_, err := d.db.ExecContext(ctx, `
			UPDATE training_jobs SET status = ?, started_at = ? WHERE id = ?
		`, status, startedAt, id)
		return err
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		completedAt = &now
		_, err := d.db.ExecContext(ctx, `
			UPDATE training_jobs
			SET status = ?, completed_at = ?, output_gguf_path = COALESCE(NULLIF(?, ''), output_gguf_path), error_message = ?
			WHERE id = ?
		`, status, completedAt, outputGGUF, errMsg, id)
		return err
	}
	_, err := d.db.ExecContext(ctx, `UPDATE training_jobs SET status = ? WHERE id = ?`, status, id)
	return err
}

func (d *DB) DeleteTrainingJob(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, "DELETE FROM training_jobs WHERE id = ?", id)
	return err
}


