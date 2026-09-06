package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

type LLMSlotMetrics struct {
	Phase           string  `json:"phase"` // "idle", "prompt_eval", "generating"
	CacheHitPct     float64 `json:"cache_hit_pct"`
	PromptTokens    int     `json:"prompt_tokens"`
	PromptCached    int     `json:"prompt_cached"`
	PromptProcessed int     `json:"prompt_processed"`
	DecodedTokens   int     `json:"decoded_tokens"`
	RemainingTokens int     `json:"remaining_tokens"`
	LiveTPS         float64 `json:"live_tps"`
	Alert           string  `json:"alert,omitempty"`
	HasAlert        bool    `json:"has_alert"`
}

type slotSample struct {
	taskID    int
	decoded   int
	timestamp time.Time
}

type SupervisorConfig struct {
	DB            *db.DB
	LogDir        string
	Host          string
	ExclusiveMode bool
}

type Supervisor struct {
	db             *db.DB
	logDir         string
	host           string
	exclusiveMode  bool
	mu             sync.Mutex
	runningCmds    map[string]*exec.Cmd
	eventChans     map[chan string]struct{}
	eventMu        sync.RWMutex
	sampleMu         sync.Mutex
	lastSample       slotSample
	liveTPS          float64
	metricsMu        sync.RWMutex
	lastMetrics      *LLMSlotMetrics
	metricsUpdated   time.Time
	activeDiffJob    *DiffusionJob
	activeDiffMu     sync.RWMutex
	activeDiffCancel context.CancelFunc
	activeTrainJob    *db.TrainingJob
	activeTrainMu     sync.RWMutex
	activeTrainCancel context.CancelFunc
}

func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.LogDir == "" {
		home, _ := os.UserHomeDir()
		cfg.LogDir = filepath.Join(home, ".llmcontrol", "logs")
	}
	_ = os.MkdirAll(cfg.LogDir, 0755)

	s := &Supervisor{
		db:            cfg.DB,
		logDir:        cfg.LogDir,
		host:          cfg.Host,
		exclusiveMode: cfg.ExclusiveMode,
		runningCmds:   make(map[string]*exec.Cmd),
		eventChans:    make(map[chan string]struct{}),
	}
	s.startBackgroundSlotPoller()
	return s
}

func (s *Supervisor) SubscribeEvents() chan string {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	ch := make(chan string, 100)
	s.eventChans[ch] = struct{}{}
	return ch
}

func (s *Supervisor) UnsubscribeEvents(ch chan string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	delete(s.eventChans, ch)
	close(ch)
}

func (s *Supervisor) broadcastEvent(event string) {
	s.eventMu.RLock()
	defer s.eventMu.RUnlock()
	for ch := range s.eventChans {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Supervisor) BuildArgs(m *db.Model, p *db.Profile, port int) []string {
	var args []string

	host := s.host
	if host == "" {
		host = "127.0.0.1"
	}
	if p.AllowLAN {
		host = "0.0.0.0"
	}

	args = append(args, "-m", m.ModelPath)
	if !strings.Contains(p.ExtraArgs, "--host") {
		args = append(args, "--host", host)
	}
	args = append(args, "--port", strconv.Itoa(port))

	// Always pass -np explicitly: llama-server may auto-scale slots when the
	// flag is omitted, multiplying KV memory and causing cross-slot cache thrash.
	parallel := p.Parallel
	if parallel <= 0 {
		parallel = 1
	}
	args = append(args, "-np", strconv.Itoa(parallel))
	if p.CtxSize > 0 {
		// With explicit -np the server treats -c as TOTAL context split
		// across slots; scale it so ctx_size stays per-request.
		args = append(args, "-c", strconv.Itoa(p.CtxSize*parallel))
	}
	if p.KVType != "" {
		args = append(args, "--cache-type-k", p.KVType, "--cache-type-v", p.KVType)
	}
	if p.FlashAttn != "" {
		args = append(args, "--flash-attn", p.FlashAttn)
	}
	// Speculative decoding / draft model resolution
	specType := p.SpecType
	if specType == "" && p.UseMTP {
		specType = "draft-mtp"
	}

	if specType != "" && specType != "none" {
		draftModel := ""
		hasDraftSpec := strings.Contains(specType, "draft-")

		if hasDraftSpec {
			if p.DraftModelPath != "" && p.DraftModelPath != m.ModelPath {
				draftModel = p.DraftModelPath
			} else if strings.Contains(specType, "draft-mtp") && m.MTPPath != "" && m.MTPPath != m.ModelPath {
				draftModel = m.MTPPath
			}
		}

		args = append(args, "--spec-type", specType)

		if draftModel != "" {
			args = append(args, "-md", draftModel)
			if p.DraftNGL > 0 {
				args = append(args, "--spec-draft-ngl", strconv.Itoa(p.DraftNGL))
			}
		}

		if p.DraftNMax > 0 {
			args = append(args, "--spec-draft-n-max", strconv.Itoa(p.DraftNMax))
		}
	}

	if p.UseVision && m.MMProjPath != "" {
		args = append(args, "--mmproj", m.MMProjPath)
	}
	if !p.EnableUI {
		args = append(args, "--no-webui")
	}
	// Tools & Web Fetch / MCP proxy
	if p.Tools != "" && p.Tools != "none" {
		if !strings.Contains(p.ExtraArgs, "--tools") {
			switch p.Tools {
			case "safe":
				args = append(args, "--tools", "read_file,file_glob_search,grep_search,get_info")
			case "all":
				args = append(args, "--tools", "all")
			default:
				args = append(args, "--tools", p.Tools)
			}
		}
		if !strings.Contains(p.ExtraArgs, "--webui-mcp-proxy") && !strings.Contains(p.ExtraArgs, "--ui-mcp-proxy") && !strings.Contains(p.ExtraArgs, "--agent") && !strings.Contains(p.ExtraArgs, "-ag") {
			args = append(args, "--webui-mcp-proxy")
		}
	}
	// Reasoning / Thinking mode
	if p.Reasoning != "" && p.Reasoning != "auto" {
		args = append(args, "--reasoning", p.Reasoning)
	}
	if p.ReasoningFormat != "" && p.ReasoningFormat != "auto" {
		args = append(args, "--reasoning-format", p.ReasoningFormat)
	}
	if p.ReasoningBudget >= 0 {
		args = append(args, "--reasoning-budget", strconv.Itoa(p.ReasoningBudget))
	}
	if p.PreserveReasoning {
		args = append(args, "--reasoning-preserve")
	}
	if p.CacheReuse > 0 {
		args = append(args, "--cache-reuse", strconv.Itoa(p.CacheReuse))
	}
	if p.ExtraArgs != "" {
		extra := strings.Fields(p.ExtraArgs)
		args = append(args, extra...)
	}

	return args
}

func (s *Supervisor) StartProcessOnly(ctx context.Context, modelID, profileName string, lanOverride ...bool) (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	model, err := s.db.GetModel(ctx, modelID)
	if err != nil || model == nil {
		return nil, fmt.Errorf("model '%s' not found", modelID)
	}

	if profileName == "" {
		profileName = model.DefaultProfile
		if profileName == "" {
			profileName = "default"
		}
	}

	var profile *db.Profile
	for i := range model.Profiles {
		if model.Profiles[i].Name == profileName {
			profile = &model.Profiles[i]
			break
		}
	}
	if profile == nil {
		profile = &db.Profile{
			ModelID:  modelID,
			Name:     profileName,
			CtxSize:  4096,
			EnableUI: true,
		}
	}
	if len(lanOverride) > 0 {
		pCopy := *profile
		pCopy.AllowLAN = lanOverride[0]
		profile = &pCopy
	}

	eng, err := s.db.GetEngine(ctx, model.EngineID)
	homeDir, _ := os.UserHomeDir()
	vkBin := filepath.Join(homeDir, "llama.cpp", "build-vk", "bin", "llama-server")
	binPath := "llama-server"
	if eng != nil && eng.BinaryPath != "" {
		binPath = eng.BinaryPath
	} else if _, err := os.Stat(vkBin); err == nil {
		binPath = vkBin
	} else if _, err := exec.LookPath("llama-server"); err != nil {
		binPath = vkBin
	}

	port := model.DefaultPort
	if port <= 0 {
		port = 8080
	}

	// Exclusive mode: stop other active models if enabled
	if s.exclusiveMode {
		active, _ := s.db.GetActiveRuntimeStates(ctx)
		for _, act := range active {
			if act.ModelID != modelID {
				_ = s.stopProcessUnlocked(ctx, act.ModelID)
			}
		}
	}

	args := s.BuildArgs(model, profile, port)
	cmd := exec.Command(binPath, args...)

	// Run in its own session so the process survives daemon restart
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Setup SYCL environment variables if running SYCL
	if strings.Contains(binPath, "sycl") || model.EngineID == "llama-sycl" {
		syclRT := filepath.Join(homeDir, ".local", "share", "oneapi-sycl-2025.3") + ":" + filepath.Join(homeDir, "llama-sycl", "llama-latest")
		cmd.Env = append(os.Environ(),
			"LD_LIBRARY_PATH="+syclRT+":"+os.Getenv("LD_LIBRARY_PATH"),
			"ONEAPI_DEVICE_SELECTOR=level_zero:0",
			"GGML_SYCL_NO_PINNED=1",
		)
	}

	logFile := filepath.Join(s.logDir, fmt.Sprintf("%s.log", modelID))
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", binPath, err)
	}

	pid := cmd.Process.Pid
	s.runningCmds[modelID] = cmd

	boundHost := "127.0.0.1"
	if profile.AllowLAN {
		boundHost = "0.0.0.0"
	}

	now := time.Now()
	_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
		ModelID:     modelID,
		PID:         pid,
		Port:        port,
		Host:        boundHost,
		ProfileName: profileName,
		Status:      "starting",
		StartedAt:   &now,
	})

	s.broadcastEvent(fmt.Sprintf("model:%s:starting", modelID))

	// Monitor process termination in background
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		delete(s.runningCmds, modelID)
		_ = s.db.SetRuntimeState(context.Background(), db.RuntimeState{
			ModelID:     modelID,
			PID:         0,
			Port:        port,
			Host:        boundHost,
			ProfileName: profileName,
			Status:      "stopped",
		})
		s.mu.Unlock()
		s.broadcastEvent(fmt.Sprintf("model:%s:stopped", modelID))
	}()

	return cmd, nil
}

func (s *Supervisor) StartModel(ctx context.Context, modelID, profileName string, lanOverride ...bool) error {
	cmd, err := s.StartProcessOnly(ctx, modelID, profileName, lanOverride...)
	if err != nil {
		return err
	}

	model, _ := s.db.GetModel(ctx, modelID)
	port := 8080
	if model != nil && model.DefaultPort > 0 {
		port = model.DefaultPort
	}

	boundHost := "127.0.0.1"
	if len(lanOverride) > 0 && lanOverride[0] {
		boundHost = "0.0.0.0"
	} else if model != nil {
		for _, p := range model.Profiles {
			if p.Name == profileName && p.AllowLAN {
				boundHost = "0.0.0.0"
				break
			}
		}
	}

	// Healthcheck loop (up to 180s for large 27B+ models)
	go func() {
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
		client := &http.Client{Timeout: 2 * time.Second}
		start := time.Now()

		for time.Since(start) < 180*time.Second {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				return
			}
			resp, err := client.Get(baseURL + "/health")
			if err == nil && (resp.StatusCode == 200 || resp.StatusCode == 503) {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					// Ready!
					now := time.Now()
					_ = s.db.SetRuntimeState(context.Background(), db.RuntimeState{
						ModelID:     modelID,
						PID:         cmd.Process.Pid,
						Port:        port,
						Host:        boundHost,
						ProfileName: profileName,
						Status:      "running",
						StartedAt:   &now,
					})
					s.broadcastEvent(fmt.Sprintf("model:%s:running", modelID))

					// Run quick benchmark
					_, _ = s.runBenchmarkOnURL(context.Background(), modelID, profileName, baseURL)
					return
				}
			}
			time.Sleep(1 * time.Second)
		}
	}()

	return nil
}

func (s *Supervisor) StopModel(ctx context.Context, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopProcessUnlocked(ctx, modelID)
}

func (s *Supervisor) stopProcessUnlocked(ctx context.Context, modelID string) error {
	m, err := s.db.GetModel(ctx, modelID)
	if err != nil || m == nil {
		return nil
	}

	if m.Runtime == nil || m.Runtime.PID == 0 {
		_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
			ModelID: modelID,
			Status:  "stopped",
		})
		return nil
	}

	pid := m.Runtime.PID
	proc, err := os.FindProcess(pid)
	if err == nil && proc != nil {
		_ = proc.Signal(syscall.SIGTERM)

		// Wait up to 5s for exit
		done := make(chan bool, 1)
		go func() {
			for i := 0; i < 25; i++ {
				if err := proc.Signal(syscall.Signal(0)); err != nil {
					done <- true
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			done <- false
		}()

		select {
		case ok := <-done:
			if !ok {
				_ = proc.Signal(syscall.SIGKILL)
			}
		case <-time.After(6 * time.Second):
			_ = proc.Signal(syscall.SIGKILL)
		}
	}

	delete(s.runningCmds, modelID)
	_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
		ModelID:     modelID,
		PID:         0,
		Port:        m.Runtime.Port,
		ProfileName: m.Runtime.ProfileName,
		Status:      "stopped",
	})

	s.broadcastEvent(fmt.Sprintf("model:%s:stopped", modelID))
	return nil
}

func (s *Supervisor) StopAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	active, err := s.db.GetActiveRuntimeStates(ctx)
	if err != nil {
		return err
	}
	for _, act := range active {
		_ = s.stopProcessUnlocked(ctx, act.ModelID)
	}
	return nil
}

func (s *Supervisor) runBenchmarkOnURL(ctx context.Context, modelID, profileName, baseURL string) (*db.BenchmarkLog, error) {
	reqBody := map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": "Скажи 7*8="},
		},
		"max_tokens":  80,
		"temperature": 0,
		"chat_template_kwargs": map[string]interface{}{
			"enable_thinking": false,
		},
	}
	bBytes, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/chat/completions", bytes.NewReader(bBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Timings struct {
			PromptN            int     `json:"prompt_n"`
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedN         int     `json:"predicted_n"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	sample := ""
	if len(result.Choices) > 0 {
		sample = strings.TrimSpace(result.Choices[0].Message.Content)
	}

	bLog := db.BenchmarkLog{
		ModelID:            modelID,
		ProfileName:        profileName,
		Timestamp:          time.Now(),
		PromptTokens:       result.Timings.PromptN,
		PromptPerSecond:    result.Timings.PromptPerSecond,
		PredictedTokens:    result.Timings.PredictedN,
		PredictedPerSecond: result.Timings.PredictedPerSecond,
		Success:            true,
		OutputSample:       sample,
	}

	_ = s.db.SaveBenchmarkLog(ctx, bLog)
	s.broadcastEvent(fmt.Sprintf("model:%s:benchmarked", modelID))

	return &bLog, nil
}

func (s *Supervisor) RunBenchmark(ctx context.Context, modelID string) (*db.BenchmarkLog, error) {
	m, err := s.db.GetModel(ctx, modelID)
	if err != nil || m == nil || m.Runtime == nil || m.Runtime.Status != "running" {
		return nil, fmt.Errorf("model '%s' is not running", modelID)
	}
	baseURL := fmt.Sprintf("http://%s:%d", s.host, m.Runtime.Port)
	return s.runBenchmarkOnURL(ctx, modelID, m.Runtime.ProfileName, baseURL)
}

func (s *Supervisor) GetLogs(modelID string, lines int) (string, error) {
	logFile := filepath.Join(s.logDir, fmt.Sprintf("%s.log", modelID))
	f, err := os.Open(logFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if lines <= 0 {
		lines = 50
	}

	var allLines []string
	buf := make([]byte, 64*1024)
	stat, _ := f.Stat()
	size := stat.Size()
	startPos := int64(0)
	if size > int64(len(buf)) {
		startPos = size - int64(len(buf))
	}
	f.Seek(startPos, io.SeekStart)
	n, _ := f.Read(buf)
	content := string(buf[:n])

	split := strings.Split(content, "\n")
	if len(split) > lines {
		allLines = split[len(split)-lines:]
	} else {
		allLines = split
	}

	return strings.Join(allLines, "\n"), nil
}

// ReattachRunning re-adopts running llama-server processes that survived a daemon restart.
// It loads runtime_state entries with status running/starting, verifies PID liveness
// and healthcheck, and starts background monitoring for re-adopted processes.
func (s *Supervisor) ReattachRunning(ctx context.Context) {
	active, err := s.db.GetActiveRuntimeStates(ctx)
	if err != nil || len(active) == 0 {
		return
	}

	client := &http.Client{Timeout: 3 * time.Second}

	for _, rt := range active {
		if !db.IsPIDAlive(rt.PID) {
			_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
				ModelID:     rt.ModelID,
				PID:         0,
				Port:        rt.Port,
				ProfileName: rt.ProfileName,
				Status:      "stopped",
			})
			continue
		}

		// PID is alive — verify healthcheck
		healthURL := fmt.Sprintf("http://%s:%d/health", s.host, rt.Port)
		resp, err := client.Get(healthURL)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			// Process exists but not responding — mark as-is, it may still be starting
			fmt.Printf("⚠️  Model '%s' (PID %d) alive but health check failed, keeping state '%s'\n", rt.ModelID, rt.PID, rt.Status)
			s.startOrphanMonitor(rt.ModelID, rt.PID, rt.Port, rt.ProfileName)
			continue
		}
		resp.Body.Close()

		// Healthy — update status to running and start monitoring
		_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
			ModelID:     rt.ModelID,
			PID:         rt.PID,
			Port:        rt.Port,
			ProfileName: rt.ProfileName,
			Status:      "running",
			StartedAt:   rt.StartedAt,
		})
		fmt.Printf("🔗 Re-attached to model '%s' (PID %d, port %d, profile '%s')\n", rt.ModelID, rt.PID, rt.Port, rt.ProfileName)
		s.broadcastEvent(fmt.Sprintf("model:%s:running", rt.ModelID))
		s.startOrphanMonitor(rt.ModelID, rt.PID, rt.Port, rt.ProfileName)
	}
}

// startOrphanMonitor watches a re-adopted process (we don't have its exec.Cmd)
// by periodically checking if the PID is still alive and whether a starting model is now healthy.
func (s *Supervisor) startOrphanMonitor(modelID string, pid, port int, profileName string) {
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		healthURL := fmt.Sprintf("http://%s:%d/health", s.host, port)

		for {
			time.Sleep(2 * time.Second)
			if !db.IsPIDAlive(pid) {
				s.mu.Lock()
				delete(s.runningCmds, modelID)
				_ = s.db.SetRuntimeState(context.Background(), db.RuntimeState{
					ModelID:     modelID,
					PID:         0,
					Port:        port,
					ProfileName: profileName,
					Status:      "stopped",
				})
				s.mu.Unlock()
				s.broadcastEvent(fmt.Sprintf("model:%s:stopped", modelID))
				return
			}

			// If model is in 'starting' state, poll /health to transition to 'running'
			m, err := s.db.GetModel(context.Background(), modelID)
			if err == nil && m != nil && m.Runtime != nil && m.Runtime.Status == "starting" {
				resp, err := client.Get(healthURL)
				if err == nil && resp.StatusCode == 200 {
					resp.Body.Close()
					now := time.Now()
					_ = s.db.SetRuntimeState(context.Background(), db.RuntimeState{
						ModelID:     modelID,
						PID:         pid,
						Port:        port,
						ProfileName: profileName,
						Status:      "running",
						StartedAt:   &now,
					})
					s.broadcastEvent(fmt.Sprintf("model:%s:running", modelID))
					go func() {
						baseURL := fmt.Sprintf("http://%s:%d", s.host, port)
						_, _ = s.runBenchmarkOnURL(context.Background(), modelID, profileName, baseURL)
					}()
				} else if resp != nil {
					resp.Body.Close()
				}
			}
		}
	}()
}

type llamaSlotNextToken struct {
	HasNextToken bool `json:"has_next_token"`
	HasNewLine   bool `json:"has_new_line"`
	NRemain      int  `json:"n_remain"`
	NDecoded     int  `json:"n_decoded"`
}

type llamaSlotItem struct {
	ID                     int                  `json:"id"`
	NCtx                   int                  `json:"n_ctx"`
	Speculative            bool                 `json:"speculative"`
	IsProcessing           bool                 `json:"is_processing"`
	IDTask                 int                  `json:"id_task"`
	NPromptTokens          int                  `json:"n_prompt_tokens"`
	NPromptTokensProcessed int                  `json:"n_prompt_tokens_processed"`
	NPromptTokensCache     int                  `json:"n_prompt_tokens_cache"`
	NextToken              []llamaSlotNextToken `json:"next_token"`
}

func (s *Supervisor) startBackgroundSlotPoller() {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			active, err := s.db.GetActiveRuntimeStates(context.Background())
			if err != nil || len(active) == 0 {
				continue
			}
			for _, act := range active {
				if act.Status == "running" && act.Port > 0 {
					_ = s.pollSlots(context.Background(), act.Port)
				}
			}
		}
	}()
}

func (s *Supervisor) pollSlots(ctx context.Context, port int) *LLMSlotMetrics {
	if port <= 0 {
		return &LLMSlotMetrics{Phase: "idle", CacheHitPct: 100}
	}

	url := fmt.Sprintf("http://%s:%d/slots", s.host, port)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return s.fallbackSlotMetrics()
	}

	resp, err := client.Do(req)
	if err != nil {
		return s.fallbackSlotMetrics()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return s.fallbackSlotMetrics()
	}

	var slots []llamaSlotItem
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return s.fallbackSlotMetrics()
	}

	now := time.Now()

	// 1. Look for active slot
	var activeSlot *llamaSlotItem
	for i := range slots {
		if slots[i].IsProcessing {
			activeSlot = &slots[i]
			break
		}
	}

	if activeSlot != nil {
		decoded := 0
		remain := 0
		if len(activeSlot.NextToken) > 0 {
			decoded = activeSlot.NextToken[0].NDecoded
			remain = activeSlot.NextToken[0].NRemain
		}

		promptTotal := activeSlot.NPromptTokens
		if promptTotal == 0 {
			promptTotal = activeSlot.NPromptTokensProcessed + activeSlot.NPromptTokensCache
		}

		cachePct := 100.0
		if promptTotal > 0 {
			cachePct = float64(activeSlot.NPromptTokensCache) / float64(promptTotal) * 100.0
		}

		phase := "prompt_eval"
		if decoded > 0 {
			phase = "generating"
		}

		hasAlert := false
		alertMsg := ""
		if promptTotal >= 4000 && cachePct < 50.0 {
			hasAlert = true
			alertMsg = fmt.Sprintf("Low cache hit: %.1f%% (%d of %d tokens)", cachePct, activeSlot.NPromptTokensCache, promptTotal)
		}

		// Calculate live TPS if generating
		liveTPS := 0.0
		s.sampleMu.Lock()
		if phase == "generating" && s.lastSample.taskID == activeSlot.IDTask && decoded > s.lastSample.decoded {
			dt := now.Sub(s.lastSample.timestamp).Seconds()
			if dt > 0.05 {
				instTPS := float64(decoded-s.lastSample.decoded) / dt
				if s.liveTPS > 0 {
					liveTPS = 0.6*instTPS + 0.4*s.liveTPS
				} else {
					liveTPS = instTPS
				}
				s.liveTPS = liveTPS
			}
		} else if phase != "generating" {
			s.liveTPS = 0.0
		} else {
			liveTPS = s.liveTPS
		}
		s.lastSample = slotSample{
			taskID:    activeSlot.IDTask,
			decoded:   decoded,
			timestamp: now,
		}
		s.sampleMu.Unlock()

		m := &LLMSlotMetrics{
			Phase:           phase,
			CacheHitPct:     cachePct,
			PromptTokens:    promptTotal,
			PromptCached:    activeSlot.NPromptTokensCache,
			PromptProcessed: activeSlot.NPromptTokensProcessed,
			DecodedTokens:   decoded,
			RemainingTokens: remain,
			LiveTPS:         liveTPS,
			Alert:           alertMsg,
			HasAlert:        hasAlert,
		}

		s.metricsMu.Lock()
		s.lastMetrics = m
		s.metricsUpdated = now
		s.metricsMu.Unlock()

		return m
	}

	// 2. Idle state: find slot with most recent task or highest prompt tokens to preserve last stats
	var mostRecent *llamaSlotItem
	for i := range slots {
		if slots[i].NPromptTokens > 0 {
			if mostRecent == nil || slots[i].IDTask > mostRecent.IDTask {
				mostRecent = &slots[i]
			}
		}
	}

	cachePct := 100.0
	promptTotal := 0
	cachedTotal := 0
	hasAlert := false
	alertMsg := ""

	if mostRecent != nil {
		promptTotal = mostRecent.NPromptTokens
		cachedTotal = mostRecent.NPromptTokensCache
		if promptTotal > 0 {
			cachePct = float64(cachedTotal) / float64(promptTotal) * 100.0
		}
		if promptTotal >= 4000 && cachePct < 50.0 {
			hasAlert = true
			alertMsg = fmt.Sprintf("Last prompt cache hit was low: %.1f%% (%d of %d tokens)", cachePct, cachedTotal, promptTotal)
		}
	} else {
		s.metricsMu.RLock()
		if s.lastMetrics != nil {
			cachePct = s.lastMetrics.CacheHitPct
			promptTotal = s.lastMetrics.PromptTokens
			cachedTotal = s.lastMetrics.PromptCached
		}
		s.metricsMu.RUnlock()
	}

	s.sampleMu.Lock()
	s.liveTPS = 0.0
	s.sampleMu.Unlock()

	m := &LLMSlotMetrics{
		Phase:           "idle",
		CacheHitPct:     cachePct,
		PromptTokens:    promptTotal,
		PromptCached:    cachedTotal,
		PromptProcessed: 0,
		DecodedTokens:   0,
		RemainingTokens: 0,
		LiveTPS:         0.0,
		Alert:           alertMsg,
		HasAlert:        hasAlert,
	}

	s.metricsMu.Lock()
	s.lastMetrics = m
	s.metricsUpdated = now
	s.metricsMu.Unlock()

	return m
}

func (s *Supervisor) GetSlotMetrics(ctx context.Context, port int) *LLMSlotMetrics {
	s.metricsMu.RLock()
	if s.lastMetrics != nil && time.Since(s.metricsUpdated) < 600*time.Millisecond {
		res := *s.lastMetrics
		s.metricsMu.RUnlock()
		return &res
	}
	s.metricsMu.RUnlock()

	return s.pollSlots(ctx, port)
}

func (s *Supervisor) fallbackSlotMetrics() *LLMSlotMetrics {
	s.metricsMu.RLock()
	defer s.metricsMu.RUnlock()
	if s.lastMetrics != nil {
		res := *s.lastMetrics
		res.Phase = "idle"
		res.LiveTPS = 0.0
		return &res
	}
	return &LLMSlotMetrics{Phase: "idle", CacheHitPct: 100}
}
