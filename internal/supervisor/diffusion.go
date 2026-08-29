package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

type ImageGenRequest struct {
	ModelID        string  `json:"model_id"`
	ProfileName    string  `json:"profile_name,omitempty"`
	Prompt         string  `json:"prompt"`
	NegativePrompt string  `json:"negative_prompt,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	CFGScale       float64 `json:"cfg_scale,omitempty"`
	Seed           int64   `json:"seed,omitempty"`
	SamplingMethod string  `json:"sampling_method,omitempty"`
	ClipOnCPU      *bool   `json:"clip_on_cpu,omitempty"`
	VAEOnCPU       *bool   `json:"vae_on_cpu,omitempty"`
	OffloadParams  *bool   `json:"offload_params,omitempty"`
	ExtraArgs      string  `json:"extra_args,omitempty"`
}

type DiffusionJob struct {
	ID         string             `json:"id"`
	ModelID    string             `json:"model_id"`
	Prompt     string             `json:"prompt"`
	Status     string             `json:"status"` // "running", "completed", "failed", "cancelled"
	Step       int                `json:"step"`
	TotalSteps int                `json:"total_steps"`
	Percent    float64            `json:"percent"`
	StatusText string             `json:"status_text"`
	Error      string             `json:"error,omitempty"`
	Image      *db.GeneratedImage `json:"image,omitempty"`
	StartTime  time.Time          `json:"start_time"`
	DurationMs int64              `json:"duration_ms"`
}

type ImageGenProgress struct {
	ID         string  `json:"id"`
	Step       int     `json:"step"`
	TotalSteps int     `json:"total_steps"`
	Percent    float64 `json:"percent"`
	StatusText string  `json:"status_text"`
}

var (
	stepRegex     = regexp.MustCompile(`(\d+)/(\d+)\s+-\s+([0-9\.]+[a-zA-Z]+/s|[0-9\.]+s/[a-zA-Z]+)`)
	samplingRegex = regexp.MustCompile(`(\d+)/(\d+)\s+-\s+([0-9\.]+s/it|[0-9\.]+it/s)`)
	tensorRegex   = regexp.MustCompile(`(\d+)/(\d+)\s+-\s+([0-9\.]+[M|G|k]B/s)`)
)

func (s *Supervisor) GetImageDir() string {
	home, _ := os.UserHomeDir()
	imgDir := filepath.Join(home, ".llmcontrol", "images")
	_ = os.MkdirAll(imgDir, 0755)
	return imgDir
}

func (s *Supervisor) GetActiveImageJob() *DiffusionJob {
	s.activeDiffMu.RLock()
	defer s.activeDiffMu.RUnlock()
	if s.activeDiffJob == nil {
		return nil
	}
	job := *s.activeDiffJob
	return &job
}

func (s *Supervisor) CancelImageGeneration() bool {
	s.activeDiffMu.Lock()
	defer s.activeDiffMu.Unlock()
	if s.activeDiffCancel != nil {
		s.activeDiffCancel()
		s.activeDiffCancel = nil
		if s.activeDiffJob != nil {
			s.activeDiffJob.Status = "cancelled"
			s.activeDiffJob.StatusText = "Отменено пользователем"
		}
		s.broadcastEvent("image:error:cancelled")
		return true
	}
	return false
}

// StartImageGenerationAsync launches image generation in a background worker and returns immediately.
func (s *Supervisor) StartImageGenerationAsync(req ImageGenRequest) (*DiffusionJob, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	s.activeDiffMu.Lock()
	if s.activeDiffJob != nil && s.activeDiffJob.Status == "running" {
		s.activeDiffMu.Unlock()
		return nil, fmt.Errorf("генерация изображения уже выполняется (ID: %s)", s.activeDiffJob.ID)
	}

	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	imageID := "img_" + hex.EncodeToString(idBytes)

	ctx, cancel := context.WithCancel(context.Background())
	s.activeDiffCancel = cancel

	job := &DiffusionJob{
		ID:         imageID,
		ModelID:    req.ModelID,
		Prompt:     req.Prompt,
		Status:     "running",
		StatusText: "Инициализация sd-cli Vulkan...",
		StartTime:  time.Now(),
	}
	s.activeDiffJob = job
	s.activeDiffMu.Unlock()

	go func() {
		defer func() {
			s.activeDiffMu.Lock()
			s.activeDiffCancel = nil
			s.activeDiffMu.Unlock()
		}()

		img, err := s.generateImageCore(ctx, imageID, req, job)
		s.activeDiffMu.Lock()
		if err != nil {
			if ctx.Err() != nil {
				job.Status = "cancelled"
				job.StatusText = "Отменено"
			} else {
				job.Status = "failed"
				job.Error = err.Error()
				job.StatusText = "Ошибка: " + err.Error()
			}
		} else {
			job.Status = "completed"
			job.StatusText = "Завершено"
			job.Percent = 100.0
			job.Image = img
			job.DurationMs = img.DurationMs
		}
		s.activeDiffMu.Unlock()
	}()

	return job, nil
}

// BuildDiffusionArgs builds the command line arguments for sd-cli.
func (s *Supervisor) BuildDiffusionArgs(m *db.Model, p *db.Profile, req ImageGenRequest, outputPath string, seed int64) []string {
	var args []string
	args = append(args, "--mode", "img_gen")
	args = append(args, "--diffusion-model", m.ModelPath)

	if m.VAEPath != "" {
		args = append(args, "--vae", m.VAEPath)
	}

	if m.CLIPPath != "" {
		lowerClip := strings.ToLower(m.CLIPPath)
		if strings.Contains(lowerClip, "qwen") || strings.Contains(lowerClip, "llm") {
			args = append(args, "--llm", m.CLIPPath)
		} else if strings.Contains(lowerClip, "t5") {
			args = append(args, "--t5xxl", m.CLIPPath)
		} else {
			args = append(args, "--clip_l", m.CLIPPath)
		}
	}

	args = append(args, "--prompt", req.Prompt)
	if req.NegativePrompt != "" {
		args = append(args, "--negative-prompt", req.NegativePrompt)
	}

	width := req.Width
	if width <= 0 && p != nil && p.Width > 0 {
		width = p.Width
	}
	if width <= 0 {
		width = 512
	}
	args = append(args, "--width", strconv.Itoa(width))

	height := req.Height
	if height <= 0 && p != nil && p.Height > 0 {
		height = p.Height
	}
	if height <= 0 {
		height = 512
	}
	args = append(args, "--height", strconv.Itoa(height))

	steps := req.Steps
	if steps <= 0 && p != nil && p.Steps > 0 {
		steps = p.Steps
	}
	if steps <= 0 {
		steps = 20
	}
	args = append(args, "--steps", strconv.Itoa(steps))

	cfgScale := req.CFGScale
	if cfgScale <= 0 && p != nil && p.CFGScale > 0 {
		cfgScale = p.CFGScale
	}
	if cfgScale <= 0 {
		cfgScale = 7.0
	}
	args = append(args, "--cfg-scale", fmt.Sprintf("%.2f", cfgScale))

	args = append(args, "--seed", strconv.FormatInt(seed, 10))

	samplingMethod := req.SamplingMethod
	if samplingMethod == "" && p != nil {
		samplingMethod = p.SamplingMethod
	}
	if samplingMethod != "" {
		args = append(args, "--sampling-method", samplingMethod)
	}

	// Memory offloading flags (critical for preventing OOM on 16GB Arc A770)
	clipOnCPU := false
	if p != nil && p.ClipOnCPU {
		clipOnCPU = true
	}
	if req.ClipOnCPU != nil {
		clipOnCPU = *req.ClipOnCPU
	}
	if clipOnCPU {
		args = append(args, "--clip-on-cpu")
	}

	vaeOnCPU := false
	if p != nil && p.VAEOnCPU {
		vaeOnCPU = true
	}
	if req.VAEOnCPU != nil {
		vaeOnCPU = *req.VAEOnCPU
	}
	if vaeOnCPU {
		args = append(args, "--vae-on-cpu")
	}

	offloadParams := false
	if p != nil && p.OffloadParams {
		offloadParams = true
	}
	if req.OffloadParams != nil {
		offloadParams = *req.OffloadParams
	}
	if offloadParams {
		args = append(args, "--offload-params-to-cpu")
	}

	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	if threads < 2 {
		threads = 2
	}
	args = append(args, "--threads", strconv.Itoa(threads))

	args = append(args, "--output", outputPath)

	if p != nil && p.ExtraArgs != "" {
		fields := strings.Fields(p.ExtraArgs)
		args = append(args, fields...)
	}
	if req.ExtraArgs != "" {
		fields := strings.Fields(req.ExtraArgs)
		args = append(args, fields...)
	}

	return args
}

// GenerateImage runs sd-cli synchronously to produce a generated image.
func (s *Supervisor) GenerateImage(ctx context.Context, req ImageGenRequest) (*db.GeneratedImage, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	imageID := "img_" + hex.EncodeToString(idBytes)

	job := &DiffusionJob{
		ID:         imageID,
		ModelID:    req.ModelID,
		Prompt:     req.Prompt,
		Status:     "running",
		StatusText: "Инициализация sd-cli Vulkan...",
		StartTime:  time.Now(),
	}

	return s.generateImageCore(ctx, imageID, req, job)
}

func (s *Supervisor) generateImageCore(ctx context.Context, imageID string, req ImageGenRequest, job *DiffusionJob) (*db.GeneratedImage, error) {
	// Exclusive mode check: stop active LLM models to free VRAM if exclusive_mode is active
	if s.exclusiveMode {
		_ = s.StopAll(ctx)
	}

	// Resolve Model
	var model *db.Model
	var err error
	if req.ModelID != "" {
		model, err = s.db.GetModel(ctx, req.ModelID)
	} else {
		// Pick first diffusion model or any model
		models, _ := s.db.ListModels(ctx)
		for i := range models {
			if models[i].ModelType == "diffusion" || strings.HasPrefix(models[i].EngineID, "sd") {
				model = &models[i]
				break
			}
		}
		if model == nil && len(models) > 0 {
			model = &models[0]
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get model: %w", err)
	}
	if model == nil {
		return nil, fmt.Errorf("no diffusion model configured")
	}

	// Resolve Engine binary
	engine, err := s.db.GetEngine(ctx, model.EngineID)
	if err != nil || engine == nil {
		// Fallback to default sd-vk path
		home, _ := os.UserHomeDir()
		sdVkPath := filepath.Join(home, ".unsloth", "vulkan-sd-cpp", "sd-cli")
		if _, statErr := os.Stat(sdVkPath); statErr == nil {
			engine = &db.Engine{
				ID:         "sd-vk",
				Name:       "Stable Diffusion Vulkan",
				BinaryPath: sdVkPath,
			}
		} else {
			return nil, fmt.Errorf("engine %q not found and default sd-cli binary not found", model.EngineID)
		}
	}

	// Resolve Profile
	var profile *db.Profile
	profileName := req.ProfileName
	if profileName == "" {
		profileName = model.DefaultProfile
	}
	if profileName != "" {
		profile, _ = s.db.GetProfile(ctx, model.ID, profileName)
	}

	// Generate Random Seed if unset
	seed := req.Seed
	if seed <= 0 {
		n, _ := rand.Int(rand.Reader, big.NewInt(1<<48))
		seed = n.Int64()
	}

	// Setup Output file
	outDir := s.GetImageDir()
	outPath := filepath.Join(outDir, imageID+".png")

	// Build Command Args
	args := s.BuildDiffusionArgs(model, profile, req, outPath, seed)

	cmd := exec.CommandContext(ctx, engine.BinaryPath, args...)

	// Setup stdout/stderr pipe for log & progress tracking
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	logPath := filepath.Join(s.logDir, fmt.Sprintf("diffusion_%s.log", imageID))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create diffusion log file: %w", err)
	}
	defer logFile.Close()

	startTime := time.Now()

	s.broadcastEvent(fmt.Sprintf("image:start:%s:%s", imageID, model.ID))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start sd-cli: %w", err)
	}

	var progressMu sync.Mutex
	tee := io.TeeReader(stdoutPipe, logFile)

	go func() {
		buf := make([]byte, 1024)
		var lineBuf []byte
		for {
			n, rErr := tee.Read(buf)
			if n > 0 {
				for i := 0; i < n; i++ {
					b := buf[i]
					if b == '\r' || b == '\n' {
						if len(lineBuf) > 0 {
							lineStr := strings.TrimSpace(string(lineBuf))
							lineBuf = lineBuf[:0]
							if matches := samplingRegex.FindStringSubmatch(lineStr); len(matches) >= 3 {
								curStep, _ := strconv.Atoi(matches[1])
								totalSteps, _ := strconv.Atoi(matches[2])
								if totalSteps > 0 {
									pct := float64(curStep) / float64(totalSteps) * 100.0
									progressMu.Lock()
									if job != nil {
										job.Step = curStep
										job.TotalSteps = totalSteps
										job.Percent = pct
										job.StatusText = fmt.Sprintf("Шаг %d из %d (%.0f%%)", curStep, totalSteps, pct)
									}
									s.broadcastEvent(fmt.Sprintf("image:progress:%s:%d:%d:%.1f", imageID, curStep, totalSteps, pct))
									progressMu.Unlock()
								}
							} else if matches := tensorRegex.FindStringSubmatch(lineStr); len(matches) >= 3 {
								curStep, _ := strconv.Atoi(matches[1])
								totalSteps, _ := strconv.Atoi(matches[2])
								if totalSteps > 0 {
									pct := float64(curStep) / float64(totalSteps) * 100.0
									progressMu.Lock()
									if job != nil {
										if job.Step > 0 && job.Step >= job.TotalSteps {
											job.StatusText = "Декодирование изображения (VAE)..."
										} else {
											job.StatusText = fmt.Sprintf("Загрузка модели (%d/%d)", curStep, totalSteps)
										}
									}
									s.broadcastEvent(fmt.Sprintf("image:load:%s:%d:%d:%.1f", imageID, curStep, totalSteps, pct))
									progressMu.Unlock()
								}
							}
						}
					} else {
						lineBuf = append(lineBuf, b)
					}
				}
			}
			if rErr != nil {
				break
			}
		}
	}()

	waitErr := cmd.Wait()
	durationMs := time.Since(startTime).Milliseconds()

	if waitErr != nil {
		s.broadcastEvent(fmt.Sprintf("image:error:%s", imageID))
		return nil, fmt.Errorf("sd-cli execution failed (%w); see log at %s", waitErr, logPath)
	}

	// Verify output file exists
	if _, err := os.Stat(outPath); err != nil {
		return nil, fmt.Errorf("sd-cli finished but output image file was not created: %w", err)
	}

	// Determine final dimensions & steps
	width := req.Width
	if width <= 0 && profile != nil && profile.Width > 0 {
		width = profile.Width
	}
	if width <= 0 {
		width = 512
	}
	height := req.Height
	if height <= 0 && profile != nil && profile.Height > 0 {
		height = profile.Height
	}
	if height <= 0 {
		height = 512
	}
	steps := req.Steps
	if steps <= 0 && profile != nil && profile.Steps > 0 {
		steps = profile.Steps
	}
	if steps <= 0 {
		steps = 20
	}
	cfgScale := req.CFGScale
	if cfgScale <= 0 && profile != nil && profile.CFGScale > 0 {
		cfgScale = profile.CFGScale
	}
	if cfgScale <= 0 {
		cfgScale = 7.0
	}

	genImg := db.GeneratedImage{
		ID:             imageID,
		Prompt:         req.Prompt,
		NegativePrompt: req.NegativePrompt,
		ModelID:        model.ID,
		ProfileName:    profileName,
		Width:          width,
		Height:         height,
		Steps:          steps,
		CFGScale:       cfgScale,
		Seed:           seed,
		DurationMs:     durationMs,
		FilePath:       outPath,
		CreatedAt:      time.Now(),
	}

	if err := s.db.SaveGeneratedImage(ctx, genImg); err != nil {
		// Log error but don't fail since image file is present
		fmt.Printf("Warning: failed to save generated image to DB: %v\n", err)
	}

	s.broadcastEvent(fmt.Sprintf("image:complete:%s:%d", imageID, durationMs))

	return &genImg, nil
}
