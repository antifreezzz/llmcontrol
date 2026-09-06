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
	ModelType      string        `json:"model_type,omitempty"` // "llm", "diffusion"
	ModelPath      string        `json:"model_path"`
	MMProjPath     string        `json:"mmproj_path,omitempty"`
	MTPPath        string        `json:"mtp_path,omitempty"`
	VAEPath        string        `json:"vae_path,omitempty"`
	CLIPPath       string        `json:"clip_path,omitempty"`
	DefaultPort    int           `json:"default_port"`
	DefaultProfile string        `json:"default_profile"`
	IsFavorite     bool          `json:"is_favorite"`
	CreatedAt      time.Time     `json:"created_at"`
	Profiles       []Profile     `json:"profiles,omitempty"`
	Runtime        *RuntimeState `json:"runtime,omitempty"`
}

type Profile struct {
	ID                int64   `json:"id,omitempty"`
	ModelID           string  `json:"model_id"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	CtxSize           int     `json:"ctx_size"`
	Parallel          int     `json:"parallel"`
	KVType            string  `json:"kv_type"`
	FlashAttn         string  `json:"flash_attn"`
	UseMTP            bool    `json:"use_mtp"`
	SpecType          string  `json:"spec_type,omitempty"`
	DraftModelPath    string  `json:"draft_model_path,omitempty"`
	DraftNMax         int     `json:"draft_n_max,omitempty"`
	DraftNGL          int     `json:"draft_ngl,omitempty"`
	UseVision         bool    `json:"use_vision"`
	EnableUI          bool    `json:"enable_ui"`
	AllowLAN          bool    `json:"allow_lan"`                    // --host 0.0.0.0 (LAN access) vs 127.0.0.1 (local only)
	Tools             string  `json:"tools"`
	Reasoning         string  `json:"reasoning,omitempty"`          // 'auto', 'on', 'off'
	ReasoningFormat   string  `json:"reasoning_format,omitempty"`   // 'auto', 'none', 'deepseek', 'deepseek-legacy'
	ReasoningBudget   int     `json:"reasoning_budget,omitempty"`   // -1 for unlimited, 0 for off, >0 for budget
	PreserveReasoning bool    `json:"preserve_reasoning,omitempty"` // --reasoning-preserve: keep think traces in full history (KV-cache friendly)
	CacheReuse        int     `json:"cache_reuse,omitempty"`        // --cache-reuse N: min chunk size for KV shift reuse, 0 = off
	ClipOnCPU         bool    `json:"clip_on_cpu,omitempty"`        // --clip-on-cpu: keep text encoder on CPU to free GPU VRAM
	VAEOnCPU          bool    `json:"vae_on_cpu,omitempty"`         // --vae-on-cpu: keep VAE decode on CPU
	OffloadParams     bool    `json:"offload_params,omitempty"`     // --offload-params-to-cpu
	Width             int     `json:"width,omitempty"`              // diffusion default width
	Height            int     `json:"height,omitempty"`             // diffusion default height
	Steps             int     `json:"steps,omitempty"`              // diffusion default steps
	CFGScale          float64 `json:"cfg_scale,omitempty"`          // diffusion default cfg scale
	SamplingMethod    string  `json:"sampling_method,omitempty"`    // diffusion sampling method (euler, euler_a, etc.)
	ExtraArgs         string  `json:"extra_args,omitempty"`
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

type GeneratedImage struct {
	ID             string    `json:"id"`
	Prompt         string    `json:"prompt"`
	NegativePrompt string    `json:"negative_prompt,omitempty"`
	ModelID        string    `json:"model_id"`
	ProfileName    string    `json:"profile_name,omitempty"`
	Width          int       `json:"width"`
	Height         int       `json:"height"`
	Steps          int       `json:"steps"`
	CFGScale       float64   `json:"cfg_scale"`
	Seed           int64     `json:"seed"`
	DurationMs     int64     `json:"duration_ms"`
	FilePath       string    `json:"file_path"`
	CreatedAt      time.Time `json:"created_at"`
}

type TrainingJob struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	BaseModelPath  string     `json:"base_model_path"`
	DatasetPath    string     `json:"dataset_path"`
	ValDatasetPath string     `json:"val_dataset_path,omitempty"`
	OutputName     string     `json:"output_name"`
	Epochs         int        `json:"epochs"`
	BatchSize      int        `json:"batch_size"`
	GradAccum      int        `json:"grad_accum"`
	LearningRate   float64    `json:"learning_rate"`
	LoraR          int        `json:"lora_r"`
	LoraAlpha      int        `json:"lora_alpha"`
	TargetModules  string     `json:"target_modules"`
	QuantType      string     `json:"quant_type"` // "q8_0", "q4_k_m", "fp16", "none"
	Status         string     `json:"status"`     // "pending", "running", "completed", "failed", "cancelled"
	CurrentStep    int        `json:"current_step"`
	TotalSteps     int        `json:"total_steps"`
	CurrentEpoch   float64    `json:"current_epoch"`
	CurrentLoss    float64    `json:"current_loss"`
	ValLoss        float64    `json:"val_loss,omitempty"`
	Accuracy       float64    `json:"accuracy,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	OutputGGUFPath string     `json:"output_gguf_path,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

