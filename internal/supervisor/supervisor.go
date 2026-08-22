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

type SupervisorConfig struct {
	DB            *db.DB
	LogDir        string
	Host          string
	ExclusiveMode bool
}

type Supervisor struct {
	db            *db.DB
	logDir        string
	host          string
	exclusiveMode bool
	mu            sync.Mutex
	runningCmds   map[string]*exec.Cmd
	eventChans    map[chan string]struct{}
	eventMu       sync.RWMutex
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

	return &Supervisor{
		db:            cfg.DB,
		logDir:        cfg.LogDir,
		host:          cfg.Host,
		exclusiveMode: cfg.ExclusiveMode,
		runningCmds:   make(map[string]*exec.Cmd),
		eventChans:    make(map[chan string]struct{}),
	}
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

	args = append(args, "-m", m.ModelPath)
	args = append(args, "--host", s.host)
	args = append(args, "--port", strconv.Itoa(port))

	if p.CtxSize > 0 {
		args = append(args, "-c", strconv.Itoa(p.CtxSize))
	}
	if p.Parallel > 1 {
		args = append(args, "-np", strconv.Itoa(p.Parallel))
	}
	if p.KVType != "" {
		args = append(args, "--cache-type-k", p.KVType, "--cache-type-v", p.KVType)
	}
	if p.FlashAttn != "" {
		args = append(args, "--flash-attn", p.FlashAttn)
	}
	if p.UseMTP && m.MTPPath != "" {
		args = append(args, "-md", m.MTPPath)
	}
	if p.UseVision && m.MMProjPath != "" {
		args = append(args, "--mmproj", m.MMProjPath)
	}
	if !p.EnableUI {
		args = append(args, "--no-webui")
	}
	if p.ExtraArgs != "" {
		extra := strings.Fields(p.ExtraArgs)
		args = append(args, extra...)
	}

	return args
}

func (s *Supervisor) StartProcessOnly(ctx context.Context, modelID, profileName string) (*exec.Cmd, error) {
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

	eng, err := s.db.GetEngine(ctx, model.EngineID)
	homeDir, _ := os.UserHomeDir()
	binPath := "llama-server"
	if _, err := exec.LookPath("llama-server"); err != nil {
		binPath = filepath.Join(homeDir, "llama.cpp", "build-vk", "bin", "llama-server")
	}
	if eng != nil && eng.BinaryPath != "" {
		binPath = eng.BinaryPath
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

	now := time.Now()
	_ = s.db.SetRuntimeState(ctx, db.RuntimeState{
		ModelID:     modelID,
		PID:         pid,
		Port:        port,
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
			ProfileName: profileName,
			Status:      "stopped",
		})
		s.mu.Unlock()
		s.broadcastEvent(fmt.Sprintf("model:%s:stopped", modelID))
	}()

	return cmd, nil
}

func (s *Supervisor) StartModel(ctx context.Context, modelID, profileName string) error {
	cmd, err := s.StartProcessOnly(ctx, modelID, profileName)
	if err != nil {
		return err
	}

	model, _ := s.db.GetModel(ctx, modelID)
	port := 8080
	if model != nil && model.DefaultPort > 0 {
		port = model.DefaultPort
	}

	// Healthcheck loop (up to 60s)
	go func() {
		baseURL := fmt.Sprintf("http://%s:%d", s.host, port)
		client := &http.Client{Timeout: 2 * time.Second}
		start := time.Now()

		for time.Since(start) < 60*time.Second {
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
