package db

import "time"

type Engine struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BinaryPath  string `json:"binary_path"`
	DefaultArgs string `json:"default_args"`
}

type Model struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	EngineID       string        `json:"engine_id"`
	ModelPath      string        `json:"model_path"`
	MMProjPath     string        `json:"mmproj_path,omitempty"`
	MTPPath        string        `json:"mtp_path,omitempty"`
	DefaultPort    int           `json:"default_port"`
	DefaultProfile string        `json:"default_profile"`
	IsFavorite     bool          `json:"is_favorite"`
	CreatedAt      time.Time     `json:"created_at"`
	Profiles       []Profile     `json:"profiles,omitempty"`
	Runtime        *RuntimeState `json:"runtime,omitempty"`
}

type Profile struct {
	ID                int64  `json:"id,omitempty"`
	ModelID           string `json:"model_id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	CtxSize           int    `json:"ctx_size"`
	Parallel          int    `json:"parallel"`
	KVType            string `json:"kv_type"`
	FlashAttn         string `json:"flash_attn"`
	UseMTP            bool   `json:"use_mtp"`
	SpecType          string `json:"spec_type,omitempty"`
	DraftModelPath    string `json:"draft_model_path,omitempty"`
	DraftNMax         int    `json:"draft_n_max,omitempty"`
	DraftNGL          int    `json:"draft_ngl,omitempty"`
	UseVision         bool   `json:"use_vision"`
	EnableUI          bool   `json:"enable_ui"`
	AllowLAN          bool   `json:"allow_lan"`                    // --host 0.0.0.0 (LAN access) vs 127.0.0.1 (local only)
	Tools             string `json:"tools"`
	Reasoning         string `json:"reasoning,omitempty"`          // 'auto', 'on', 'off'
	ReasoningFormat   string `json:"reasoning_format,omitempty"`   // 'auto', 'none', 'deepseek', 'deepseek-legacy'
	ReasoningBudget   int    `json:"reasoning_budget,omitempty"`   // -1 for unlimited, 0 for off, >0 for budget
	PreserveReasoning bool   `json:"preserve_reasoning,omitempty"` // --reasoning-preserve: keep think traces in full history (KV-cache friendly)
	CacheReuse        int    `json:"cache_reuse,omitempty"`        // --cache-reuse N: min chunk size for KV shift reuse, 0 = off
	ExtraArgs         string `json:"extra_args,omitempty"`
}

type RuntimeState struct {
	ModelID     string     `json:"model_id"`
	PID         int        `json:"pid"`
	Port        int        `json:"port"`
	Host        string     `json:"host,omitempty"` // '127.0.0.1' or '0.0.0.0'
	ProfileName string     `json:"profile_name"`
	Status      string     `json:"status"` // 'stopped', 'starting', 'running', 'error'
	StartedAt   *time.Time `json:"started_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type BenchmarkLog struct {
	ID                 int64     `json:"id,omitempty"`
	ModelID            string    `json:"model_id"`
	ProfileName        string    `json:"profile_name"`
	Timestamp          time.Time `json:"timestamp"`
	PromptTokens       int       `json:"prompt_tokens"`
	PromptPerSecond    float64   `json:"prompt_per_second"`
	PredictedTokens    int       `json:"predicted_tokens"`
	PredictedPerSecond float64   `json:"predicted_per_second"`
	Success            bool      `json:"success"`
	OutputSample       string    `json:"output_sample"`
}
