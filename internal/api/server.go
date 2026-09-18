package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/config"
	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/downloader"
	"github.com/antifreezzz/llmcontrol/internal/stt"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
	"github.com/antifreezzz/llmcontrol/internal/tunnel"
)

type Server struct {
	db                 *db.DB
	supervisor         *supervisor.Supervisor
	downloader         *downloader.Manager
	staticFS           fs.FS
	mux                *http.ServeMux
	sttService         *stt.Service
	tunnelMgr          *tunnel.ClientManager
	configPath         string
	wakeOnRequest      bool
	idleTimeoutSeconds int
	lastActivityUnix   int64
	// Per-model activity bookkeeping. Traffic sent straight to the model port
	// never reaches the daemon, so each model is tracked on its own signals.
	activityMu    sync.Mutex
	modelActivity map[string]int64
	modelWorkSig  map[string]string
	idleTicker    *time.Ticker
	idleStopChan  chan struct{}
}

type StatusResponse struct {
	Status      string                     `json:"status"` // 'running', 'idle'
	ActiveModel string                     `json:"active_model,omitempty"`
	Port        int                        `json:"port,omitempty"`
	Profile     string                     `json:"profile,omitempty"`
	TPS         float64                    `json:"tps,omitempty"`
	LLMMetrics  *supervisor.LLMSlotMetrics `json:"llm_metrics,omitempty"`
	System      supervisor.SystemStatus    `json:"system"`
}

func NewServer(database *db.DB, sup *supervisor.Supervisor, dl *downloader.Manager, staticFS fs.FS) *Server {
	if dl == nil {
		dl = downloader.NewManager("", database)
	}
	s := &Server{
		db:                 database,
		supervisor:         sup,
		downloader:         dl,
		staticFS:           staticFS,
		mux:                http.NewServeMux(),
		wakeOnRequest:      true,
		idleTimeoutSeconds: 300,
		lastActivityUnix:   time.Now().Unix(),
		idleStopChan:       make(chan struct{}),
		modelActivity:      make(map[string]int64),
		modelWorkSig:       make(map[string]string),
	}
	s.registerRoutes()
	s.startIdleChecker()
	return s
}

func (s *Server) SetSTTService(svc *stt.Service) {
	s.sttService = svc
}

func (s *Server) SetTunnelManager(tm *tunnel.ClientManager) {
	s.tunnelMgr = tm
}

func (s *Server) SetOnDemandConfig(wake bool, idleSeconds int) {
	s.wakeOnRequest = wake
	s.idleTimeoutSeconds = idleSeconds
}

func (s *Server) SetConfigPath(p string) {
	s.configPath = p
}

// markModelActivity records that modelID served something just now.
func (s *Server) markModelActivity(modelID string) {
	if modelID == "" {
		return
	}
	s.activityMu.Lock()
	s.modelActivity[modelID] = time.Now().Unix()
	s.activityMu.Unlock()
}

// modelActivityAt returns the last activity stamp recorded for modelID.
func (s *Server) modelActivityAt(modelID string) (int64, bool) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	v, ok := s.modelActivity[modelID]
	return v, ok
}

// clearModelActivity forgets a model that is no longer running.
func (s *Server) clearModelActivity(modelID string) {
	s.activityMu.Lock()
	delete(s.modelActivity, modelID)
	delete(s.modelWorkSig, modelID)
	s.activityMu.Unlock()
}

// modelWorkChanged stores the llama-server state signature for modelID and
// reports whether the server did anything since the previous observation. This
// is what makes a short request on the model port visible to the idle checker
// even when it finished between two polls.
func (s *Server) modelWorkChanged(modelID, sig string) bool {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	prev, ok := s.modelWorkSig[modelID]
	s.modelWorkSig[modelID] = sig
	return ok && prev != sig
}

func (s *Server) Close() {
	if s.idleTicker != nil {
		s.idleTicker.Stop()
	}
	if s.idleStopChan != nil {
		close(s.idleStopChan)
		s.idleStopChan = nil
	}
}

func (s *Server) Router() http.Handler {
	return s.corsMiddleware(s.mux)
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	// API routes
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/engines", s.handleListEngines)
	s.mux.HandleFunc("GET /api/models", s.handleListModels)
	s.mux.HandleFunc("POST /api/models", s.handleCreateModel)
	s.mux.HandleFunc("GET /api/models/{id}", s.handleGetModel)
	s.mux.HandleFunc("PUT /api/models/{id}", s.handleUpdateModel)
	s.mux.HandleFunc("DELETE /api/models/{id}", s.handleDeleteModel)
	s.mux.HandleFunc("POST /api/models/{id}/profiles", s.handleSaveProfile)
	s.mux.HandleFunc("DELETE /api/models/{id}/profiles/{name}", s.handleDeleteProfile)
	s.mux.HandleFunc("POST /api/models/{id}/start", s.handleStartModel)
	s.mux.HandleFunc("POST /api/models/{id}/stop", s.handleStopModel)
	s.mux.HandleFunc("POST /api/models/{id}/bench", s.handleBenchModel)
	s.mux.HandleFunc("GET /api/models/{id}/logs", s.handleGetLogs)
	s.mux.HandleFunc("POST /api/models/{id}/favorite", s.handleToggleFavorite)
	s.mux.HandleFunc("POST /api/models/stop-all", s.handleStopAll)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)

	// Downloader routes
	s.mux.HandleFunc("GET /api/downloader/inspect", s.handleDownloaderInspect)
	s.mux.HandleFunc("POST /api/downloader/start", s.handleDownloaderStart)
	s.mux.HandleFunc("GET /api/downloader/jobs", s.handleDownloaderJobs)
	s.mux.HandleFunc("POST /api/downloader/jobs/{id}/cancel", s.handleDownloaderCancel)

	// Image Generation routes
	s.mux.HandleFunc("POST /api/images/generate", s.handleGenerateImage)
	s.mux.HandleFunc("GET /api/images/active", s.handleGetActiveImageJob)
	s.mux.HandleFunc("POST /api/images/cancel", s.handleCancelImage)
	s.mux.HandleFunc("GET /api/images", s.handleListImages)
	s.mux.HandleFunc("GET /api/images/{id}", s.handleGetImage)
	s.mux.HandleFunc("GET /api/images/{id}/file", s.handleGetImageFile)
	s.mux.HandleFunc("DELETE /api/images/{id}", s.handleDeleteImage)

	// Training Studio routes
	s.mux.HandleFunc("POST /api/training/jobs", s.handleStartTraining)
	s.mux.HandleFunc("GET /api/training/jobs", s.handleListTrainingJobs)
	s.mux.HandleFunc("GET /api/training/jobs/{id}", s.handleGetTrainingJob)
	s.mux.HandleFunc("GET /api/training/active", s.handleGetActiveTraining)
	s.mux.HandleFunc("POST /api/training/cancel", s.handleCancelTraining)

	// OpenAI Reverse Proxy & STT
	s.mux.HandleFunc("POST /v1/audio/transcriptions", s.handleWhisperSTT)
	s.mux.HandleFunc("POST /api/stt", s.handleWhisperSTT)
	s.mux.HandleFunc("/v1/", s.handleOpenAIProxy)

	// Tunnel routes
	s.mux.HandleFunc("GET /api/tunnel/status", s.handleTunnelStatus)
	s.mux.HandleFunc("POST /api/tunnel/start", s.handleTunnelStart)
	s.mux.HandleFunc("POST /api/tunnel/stop", s.handleTunnelStop)
	s.mux.HandleFunc("POST /api/tunnel/config", s.handleTunnelConfig)

	// On-Demand routes
	s.mux.HandleFunc("GET /api/ondemand", s.handleGetOnDemand)
	s.mux.HandleFunc("POST /api/ondemand", s.handleUpdateOnDemand)

	// Web static files
	if s.staticFS != nil {
		fileServer := http.FileServer(http.FS(s.staticFS))
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
				http.NotFound(w, r)
				return
			}
			fileServer.ServeHTTP(w, r)
		})
	}
}

func (s *Server) buildStatusResponse(ctx context.Context) StatusResponse {
	active, err := s.db.GetActiveRuntimeStates(ctx)
	sysStat := supervisor.GetSystemStatus()

	resp := StatusResponse{
		Status: "idle",
		System: sysStat,
	}

	if err == nil && len(active) > 0 {
		act := active[0]
		resp.Status = act.Status
		resp.ActiveModel = act.ModelID
		resp.Port = act.Port
		resp.Profile = act.ProfileName

		latest, _ := s.db.GetLatestBenchmark(ctx, act.ModelID)
		if latest != nil {
			resp.TPS = latest.PredictedPerSecond
		}

		if act.Status == "running" && act.Port > 0 {
			metrics := s.supervisor.GetSlotMetrics(ctx, act.Port)
			if metrics != nil {
				resp.LLMMetrics = metrics
				if metrics.LiveTPS > 0 {
					resp.TPS = metrics.LiveTPS
				}
			}
		}
	}
	return resp
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := s.buildStatusResponse(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleListEngines(w http.ResponseWriter, r *http.Request) {
	engines, err := s.db.ListEngines(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(engines)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.db.ListModels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string       `json:"id"`
		Name           string       `json:"name"`
		EngineID       string       `json:"engine_id"`
		ModelPath      string       `json:"model_path"`
		MMProjPath     string       `json:"mmproj_path"`
		MTPPath        string       `json:"mtp_path"`
		DefaultPort    int          `json:"default_port"`
		DefaultProfile string       `json:"default_profile"`
		IsFavorite     *bool        `json:"is_favorite"`
		Profiles       []db.Profile `json:"profiles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" || req.Name == "" || req.ModelPath == "" {
		http.Error(w, "id, name and model_path are required", http.StatusBadRequest)
		return
	}
	if req.EngineID == "" {
		req.EngineID = "llama-vk"
	}
	if req.DefaultPort <= 0 {
		req.DefaultPort = 8080
	}
	if req.DefaultProfile == "" {
		req.DefaultProfile = "default"
	}

	ctx := r.Context()
	existing, _ := s.db.GetModel(ctx, req.ID)
	isFav := false
	if req.IsFavorite != nil {
		isFav = *req.IsFavorite
	} else if existing != nil {
		isFav = existing.IsFavorite
	}

	m := db.Model{
		ID:             req.ID,
		Name:           req.Name,
		EngineID:       req.EngineID,
		ModelPath:      req.ModelPath,
		MMProjPath:     req.MMProjPath,
		MTPPath:        req.MTPPath,
		DefaultPort:    req.DefaultPort,
		DefaultProfile: req.DefaultProfile,
		IsFavorite:     isFav,
	}

	if err := s.db.SaveModel(ctx, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, p := range req.Profiles {
		p.ModelID = m.ID
		if p.CtxSize <= 0 {
			p.CtxSize = 4096
		}
		_ = s.db.SaveProfile(ctx, p)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "created", "model_id": m.ID})
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	existing, err := s.db.GetModel(ctx, id)
	if err != nil || existing == nil {
		http.NotFound(w, r)
		return
	}

	var req struct {
		Name           string       `json:"name"`
		EngineID       string       `json:"engine_id"`
		ModelPath      string       `json:"model_path"`
		MMProjPath     string       `json:"mmproj_path"`
		MTPPath        string       `json:"mtp_path"`
		DefaultPort    int          `json:"default_port"`
		DefaultProfile string       `json:"default_profile"`
		IsFavorite     *bool        `json:"is_favorite"`
		Profiles       []db.Profile `json:"profiles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	m := db.Model{
		ID:             id,
		Name:           req.Name,
		EngineID:       req.EngineID,
		ModelPath:      req.ModelPath,
		MMProjPath:     req.MMProjPath,
		MTPPath:        req.MTPPath,
		DefaultPort:    req.DefaultPort,
		DefaultProfile: req.DefaultProfile,
	}

	if m.Name == "" {
		m.Name = existing.Name
	}
	if m.EngineID == "" {
		m.EngineID = existing.EngineID
	}
	if m.ModelPath == "" {
		m.ModelPath = existing.ModelPath
	}
	if m.DefaultPort <= 0 {
		m.DefaultPort = existing.DefaultPort
	}
	if m.DefaultProfile == "" {
		m.DefaultProfile = existing.DefaultProfile
	}
	if req.IsFavorite != nil {
		m.IsFavorite = *req.IsFavorite
	} else {
		m.IsFavorite = existing.IsFavorite
	}

	if err := s.db.SaveModel(ctx, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(req.Profiles) > 0 {
		for _, p := range req.Profiles {
			p.ModelID = id
			if p.CtxSize <= 0 {
				p.CtxSize = 4096
			}
			_ = s.db.SaveProfile(ctx, p)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated", "model_id": id})
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	_ = s.supervisor.StopModel(ctx, id)
	if err := s.db.DeleteModel(ctx, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "model_id": id})
}

func (s *Server) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p db.Profile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if p.Name == "" {
		http.Error(w, "profile name is required", http.StatusBadRequest)
		return
	}
	p.ModelID = id
	if p.CtxSize <= 0 {
		p.CtxSize = 4096
	}

	if err := s.db.SaveProfile(r.Context(), p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "saved", "profile": p.Name})
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")

	if err := s.db.DeleteProfile(r.Context(), id, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "profile": name})
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.db.GetModel(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func (s *Server) handleStartModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Profile  string `json:"profile"`
		AllowLAN *bool  `json:"allow_lan,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	profile := req.Profile
	if profile == "" {
		profile = r.URL.Query().Get("profile")
	}

	var lanOverride []bool
	if req.AllowLAN != nil {
		lanOverride = []bool{*req.AllowLAN}
	} else if lanQ := r.URL.Query().Get("lan"); lanQ != "" {
		lanOverride = []bool{lanQ == "1" || lanQ == "true"}
	} else if lanQ := r.URL.Query().Get("allow_lan"); lanQ != "" {
		lanOverride = []bool{lanQ == "1" || lanQ == "true"}
	}

	if err := s.supervisor.StartModel(r.Context(), id, profile, lanOverride...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Starting a model counts as activity, otherwise the idle checker would
	// auto-stop a freshly started model as soon as the daemon uptime exceeds
	// idle_timeout_seconds.
	atomic.StoreInt64(&s.lastActivityUnix, time.Now().Unix())
	s.markModelActivity(id)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting", "model_id": id})
}

func (s *Server) handleStopModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.supervisor.StopModel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.clearModelActivity(id)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "stopped", "model_id": id})
}

func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	if err := s.supervisor.StopAll(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "stopped_all"})
}

func (s *Server) handleBenchModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.supervisor.RunBenchmark(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	linesStr := r.URL.Query().Get("lines")
	lines := 50
	if n, err := strconv.Atoi(linesStr); err == nil && n > 0 {
		lines = n
	}

	logs, err := s.supervisor.GetLogs(id, lines)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

func (s *Server) handleToggleFavorite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := s.db.SetFavorite(r.Context(), id, req.Favorite); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"model_id": id, "is_favorite": req.Favorite})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events := s.supervisor.SubscribeEvents()
	defer s.supervisor.UnsubscribeEvents(events)

	ctx := r.Context()

	sendTelemetry := func() {
		status := s.buildStatusResponse(ctx)
		payload, err := json.Marshal(map[string]interface{}{
			"type":   "telemetry",
			"status": status,
		})
		if err == nil {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}

	// Send initial snapshot immediately upon connection
	sendTelemetry()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	idleTickCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if strings.HasPrefix(ev, "image:progress:") {
				parts := strings.Split(ev, ":")
				if len(parts) >= 6 {
					step, _ := strconv.Atoi(parts[3])
					total, _ := strconv.Atoi(parts[4])
					pct, _ := strconv.ParseFloat(parts[5], 64)
					payload, _ := json.Marshal(map[string]interface{}{
						"type":    "image_progress",
						"id":      parts[2],
						"step":    step,
						"total":   total,
						"percent": pct,
					})
					fmt.Fprintf(w, "data: %s\n\n", payload)
					flusher.Flush()
					continue
				}
			}
			if strings.HasPrefix(ev, "image:load:") {
				parts := strings.Split(ev, ":")
				if len(parts) >= 6 {
					step, _ := strconv.Atoi(parts[3])
					total, _ := strconv.Atoi(parts[4])
					pct, _ := strconv.ParseFloat(parts[5], 64)
					payload, _ := json.Marshal(map[string]interface{}{
						"type":    "image_load",
						"id":      parts[2],
						"step":    step,
						"total":   total,
						"percent": pct,
					})
					fmt.Fprintf(w, "data: %s\n\n", payload)
					flusher.Flush()
					continue
				}
			}
			if strings.HasPrefix(ev, "image:start:") || strings.HasPrefix(ev, "image:complete:") || strings.HasPrefix(ev, "image:error:") {
				parts := strings.Split(ev, ":")
				payload, _ := json.Marshal(map[string]interface{}{
					"type":  parts[0] + "_" + parts[1],
					"id":    parts[2],
					"event": ev,
				})
				fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
				continue
			}

			evPayload, _ := json.Marshal(map[string]interface{}{
				"type":  "event",
				"event": ev,
			})
			fmt.Fprintf(w, "data: %s\n\n", evPayload)
			flusher.Flush()
			sendTelemetry()
		case <-ticker.C:
			idleTickCount++
			if idleTickCount >= 4 { // 4 * 250ms = 1.0s in idle
				idleTickCount = 0
				status := s.buildStatusResponse(ctx)
				payload, err := json.Marshal(map[string]interface{}{
					"type":   "telemetry",
					"status": status,
				})
				if err == nil {
					fmt.Fprintf(w, "data: %s\n\n", payload)
					flusher.Flush()
				}
			} else {
				status := s.buildStatusResponse(ctx)
				isBusy := status.LLMMetrics != nil && (status.LLMMetrics.Phase == "prompt_eval" || status.LLMMetrics.Phase == "generating")
				if isBusy {
					idleTickCount = 0
					payload, err := json.Marshal(map[string]interface{}{
						"type":   "telemetry",
						"status": status,
					})
					if err == nil {
						fmt.Fprintf(w, "data: %s\n\n", payload)
						flusher.Flush()
					}
				}
			}
		}
	}
}

func (s *Server) handleDownloaderInspect(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		http.Error(w, `{"error": "repo parameter is required"}`, http.StatusBadRequest)
		return
	}
	info, err := s.downloader.InspectRepo(r.Context(), repo)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleDownloaderStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoID       string `json:"repo_id"`
		Filename     string `json:"filename"`
		AutoRegister bool   `json:"auto_register"`
		ModelID      string `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "invalid json"}`, http.StatusBadRequest)
		return
	}
	job, err := s.downloader.StartDownload(req.RepoID, req.Filename, req.AutoRegister, req.ModelID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job)
}

func (s *Server) handleDownloaderJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.downloader.ListJobs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jobs)
}

func (s *Server) handleDownloaderCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.downloader.CancelJob(id); err != nil {
		http.Error(w, `{"error": "job not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

func (s *Server) handleGenerateImage(w http.ResponseWriter, r *http.Request) {
	var req supervisor.ImageGenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json: " + err.Error()})
		return
	}

	job, err := s.supervisor.StartImageGenerationAsync(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(job)
}

func (s *Server) handleGetActiveImageJob(w http.ResponseWriter, r *http.Request) {
	job := s.supervisor.GetActiveImageJob()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job)
}

func (s *Server) handleCancelImage(w http.ResponseWriter, r *http.Request) {
	cancelled := s.supervisor.CancelImageGeneration()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": cancelled})
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")
	limit := 50
	offset := 0
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
	}
	if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
		offset = o
	}

	images, err := s.db.ListGeneratedImages(r.Context(), limit, offset)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if images == nil {
		images = []db.GeneratedImage{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(images)
}

func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "generate" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Method Not Allowed. Use POST /api/images/generate to generate images.",
		})
		return
	}

	img, err := s.db.GetGeneratedImage(r.Context(), id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if img == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "image not found"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(img)
}

func (s *Server) handleGetImageFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	img, err := s.db.GetGeneratedImage(r.Context(), id)
	if err != nil || img == nil {
		http.Error(w, `{"error": "image not found"}`, http.StatusNotFound)
		return
	}

	if _, err := os.Stat(img.FilePath); err != nil {
		http.Error(w, `{"error": "file missing on disk"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, img.FilePath)
}

func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	img, _ := s.db.GetGeneratedImage(r.Context(), id)
	if img != nil && img.FilePath != "" {
		_ = os.Remove(img.FilePath)
	}

	if err := s.db.DeleteGeneratedImage(r.Context(), id); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func (s *Server) handleStartTraining(w http.ResponseWriter, r *http.Request) {
	var req supervisor.TrainingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	if req.BaseModelPath == "" || req.DatasetPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "base_model_path and dataset_path are required"})
		return
	}

	job, err := s.supervisor.StartTraining(r.Context(), req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(job)
}

func (s *Server) handleListTrainingJobs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")
	limit := 50
	offset := 0
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
	}
	if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
		offset = o
	}

	jobs, err := s.db.ListTrainingJobs(r.Context(), limit, offset)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if jobs == nil {
		jobs = []db.TrainingJob{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jobs)
}

func (s *Server) handleGetTrainingJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.db.GetTrainingJob(r.Context(), id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if job == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "training job not found"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(job)
}

func (s *Server) handleGetActiveTraining(w http.ResponseWriter, r *http.Request) {
	job := s.supervisor.GetActiveTrainingJob()
	w.Header().Set("Content-Type", "application/json")
	if job == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"active": false, "job": nil})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"active": true, "job": job})
}

func (s *Server) handleCancelTraining(w http.ResponseWriter, r *http.Request) {
	cancelled := s.supervisor.CancelTraining()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancelled": cancelled})
}

// ----------------------------------------------------------------------------
// OpenAI Reverse Proxy & On-Demand Execution
// ----------------------------------------------------------------------------

func (s *Server) handleOpenAIProxy(w http.ResponseWriter, r *http.Request) {
	// Handle GET /v1/models even if no model is running
	if r.Method == http.MethodGet && strings.TrimSuffix(r.URL.Path, "/") == "/v1/models" {
		active, _ := s.db.GetActiveRuntimeStates(r.Context())
		hasRunning := false
		for _, a := range active {
			if a.Status == "running" && a.Port > 0 {
				hasRunning = true
				break
			}
		}
		if !hasRunning {
			models, err := s.db.ListModels(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			type ModelItem struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				OwnedBy string `json:"owned_by"`
			}
			var items []ModelItem
			for _, m := range models {
				items = append(items, ModelItem{
					ID:      m.ID,
					Object:  "model",
					Created: time.Now().Unix(),
					OwnedBy: "llmcontrol",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data":   items,
			})
			return
		}
	}

	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	var reqBody struct {
		Model string `json:"model"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &reqBody)
	}

	// 1. Determine desired model and profile
	desiredModelID := ""
	desiredProfile := ""

	// Priority 1: Check if request explicitly specified a registered model in DB
	if reqBody.Model != "" && reqBody.Model != "auto" {
		if m, _ := s.db.GetModel(r.Context(), reqBody.Model); m != nil {
			desiredModelID = m.ID
			desiredProfile = m.DefaultProfile
		}
	}

	// Priority 2: If model wasn't explicitly matched from request, check VPS tunnel target model
	if desiredModelID == "" && s.tunnelMgr != nil {
		st := s.tunnelMgr.GetStatus()
		if st.TargetModelID != "" && st.TargetModelID != "auto" {
			if m, _ := s.db.GetModel(r.Context(), st.TargetModelID); m != nil {
				desiredModelID = m.ID
				desiredProfile = st.TargetProfile
				if desiredProfile == "" {
					desiredProfile = m.DefaultProfile
				}
			}
		}
	}

	// Find active running model
	active, _ := s.db.GetActiveRuntimeStates(r.Context())
	var runningModelID string
	var runningPort int
	for _, a := range active {
		if a.Status == "running" && a.Port > 0 {
			runningModelID = a.ModelID
			runningPort = a.Port
			break
		}
	}

	// Priority 3: If no specific model is targeted, but some model is already running, use it
	if desiredModelID == "" && runningPort > 0 {
		desiredModelID = runningModelID
	}

	// Priority 4: Fallback to favorite model, or the first available model
	if desiredModelID == "" {
		models, _ := s.db.ListModels(r.Context())
		for _, m := range models {
			if m.IsFavorite {
				desiredModelID = m.ID
				desiredProfile = m.DefaultProfile
				break
			}
		}
		if desiredModelID == "" && len(models) > 0 {
			desiredModelID = models[0].ID
			desiredProfile = models[0].DefaultProfile
		}
	}

	var activePort int
	// If the desired model is already running, use its port directly
	if runningPort > 0 && runningModelID == desiredModelID {
		activePort = runningPort
	} else {
		// Need to start or switch to desiredModelID
		if desiredModelID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"message": "No model available to handle request",
					"type":    "server_error",
				},
			})
			return
		}

		if !s.wakeOnRequest {
			if runningPort > 0 {
				activePort = runningPort
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"message": "No model is running and wake_on_request is disabled",
						"type":    "server_error",
					},
				})
				return
			}
		} else {
			log.Printf("[ondemand] waking up model %s (profile: %s) for incoming request...", desiredModelID, desiredProfile)
			if err := s.supervisor.StartModel(r.Context(), desiredModelID, desiredProfile); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("Failed to wake model %s: %v", desiredModelID, err),
						"type":    "server_error",
					},
				})
				return
			}

			model, _ := s.db.GetModel(r.Context(), desiredModelID)
			expectedPort := 8080
			if model != nil && model.DefaultPort > 0 {
				expectedPort = model.DefaultPort
			}

			ready := false
			for i := 0; i < 60; i++ {
				time.Sleep(800 * time.Millisecond)
				client := &http.Client{Timeout: 500 * time.Millisecond}
				resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", expectedPort))
				if err == nil && resp.StatusCode == 200 {
					resp.Body.Close()
					ready = true
					activePort = expectedPort
					break
				}
				if resp != nil {
					resp.Body.Close()
				}
			}

			if !ready {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusGatewayTimeout)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"message": fmt.Sprintf("Timed out waiting for model %s to load and start", desiredModelID),
						"type":    "server_error",
					},
				})
				return
			}
		}
	}

	// Count the request against the model that serves it.
	s.markModelActivity(desiredModelID)

	// Update last activity timestamp for idle auto-stop
	atomic.StoreInt64(&s.lastActivityUnix, time.Now().Unix())

	// Proxy to llama-server
	targetURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", activePort))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.ServeHTTP(w, r)
}

func (s *Server) startIdleChecker() {
	s.idleTicker = time.NewTicker(10 * time.Second)

	go func() {
		for {
			select {
			case <-s.idleStopChan:
				return
			case <-s.idleTicker.C:
				timeout := s.idleTimeoutSeconds
				if timeout <= 0 {
					continue
				}

				active, err := s.db.GetActiveRuntimeStates(context.Background())
				if err != nil || len(active) == 0 {
					continue
				}

				for _, act := range active {
					if act.Status != "running" || act.Port <= 0 {
						continue
					}

					// Traffic on the model port never reaches the daemon proxy, so ask
					// llama-server itself what it is doing: work in progress or any
					// state change counts as activity.
					if sig, busy, ok := s.supervisor.ServerActivity(context.Background(), act.Port); ok {
						if busy || s.modelWorkChanged(act.ModelID, sig) {
							s.markModelActivity(act.ModelID)
						}
					}

					last, known := s.modelActivityAt(act.ModelID)
					if !known {
						switch {
						case act.StartedAt != nil:
							last, known = act.StartedAt.Unix(), true
						default:
							if g := atomic.LoadInt64(&s.lastActivityUnix); g > 0 {
								last, known = g, true
							}
						}
					}
					if !known || time.Now().Unix()-last <= int64(timeout) {
						continue
					}

					log.Printf("[ondemand] idle timeout expired (%ds without activity) for model %s, auto-stopping...", timeout, act.ModelID)
					_ = s.supervisor.StopModel(context.Background(), act.ModelID)
					s.clearModelActivity(act.ModelID)
				}
			}
		}
	}()
}

// ----------------------------------------------------------------------------
// Whisper STT Endpoint (OpenAI-compatible)
// ----------------------------------------------------------------------------

func (s *Server) handleWhisperSTT(w http.ResponseWriter, r *http.Request) {
	if s.sttService == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Whisper STT service not initialized"})
		return
	}

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("failed to parse multipart: %v", err)})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing 'file' form field in multipart request"})
		return
	}
	defer file.Close()

	language := r.FormValue("language")
	if language == "" {
		language = "auto"
	}

	atomic.StoreInt64(&s.lastActivityUnix, time.Now().Unix())

	text, err := s.sttService.Transcribe(r.Context(), file, header.Filename, language)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"text": text,
	})
}

// ----------------------------------------------------------------------------
// Tunnel API Endpoints
// ----------------------------------------------------------------------------

func (s *Server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.tunnelMgr == nil {
		_ = json.NewEncoder(w).Encode(tunnel.Status{
			IsActive: false,
			State:    "disabled",
		})
		return
	}
	_ = json.NewEncoder(w).Encode(s.tunnelMgr.GetStatus())
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.tunnelMgr == nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "tunnel manager not initialized"})
		return
	}
	var req struct {
		ModelID string `json:"model_id"`
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := s.tunnelMgr.Start(req.ModelID, req.Profile); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	cfgPath := config.GetConfigPath(s.configPath)
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		cfg.VPSTargetModel = req.ModelID
		_ = config.SaveConfig(cfgPath, cfg)
	}

	_ = json.NewEncoder(w).Encode(s.tunnelMgr.GetStatus())
}

func (s *Server) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.tunnelMgr != nil {
		s.tunnelMgr.Stop()
		_ = json.NewEncoder(w).Encode(s.tunnelMgr.GetStatus())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"state": "disconnected"})
}

func (s *Server) handleTunnelConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.tunnelMgr == nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "tunnel manager not initialized"})
		return
	}
	var req struct {
		VPSHost       string `json:"vps_host"`
		VPSTunnelPort int    `json:"vps_tunnel_port"`
		VPSRemotePort int    `json:"vps_remote_port"`
		VPSToken      string `json:"vps_token"`
		TargetModelID string `json:"target_model_id"`
		TargetProfile string `json:"target_profile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	s.tunnelMgr.UpdateConfig(req.VPSHost, req.VPSTunnelPort, req.VPSRemotePort, req.VPSToken, req.TargetModelID, req.TargetProfile)

	cfgPath := config.GetConfigPath(s.configPath)
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if req.VPSHost != "" {
			cfg.VPSHost = req.VPSHost
		}
		if req.VPSTunnelPort > 0 {
			cfg.VPSTunnelPort = req.VPSTunnelPort
		}
		if req.VPSRemotePort > 0 {
			cfg.VPSRemotePort = req.VPSRemotePort
		}
		if req.VPSToken != "" {
			cfg.VPSToken = req.VPSToken
		}
		cfg.VPSTargetModel = req.TargetModelID
		_ = config.SaveConfig(cfgPath, cfg)
	}

	_ = json.NewEncoder(w).Encode(s.tunnelMgr.GetStatus())
}

// ----------------------------------------------------------------------------
// On-Demand API Endpoints
// ----------------------------------------------------------------------------

func (s *Server) handleGetOnDemand(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"wake_on_request":      s.wakeOnRequest,
		"idle_timeout_seconds": s.idleTimeoutSeconds,
	})
}

func (s *Server) handleUpdateOnDemand(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		WakeOnRequest      *bool `json:"wake_on_request"`
		IdleTimeoutSeconds *int  `json:"idle_timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if req.WakeOnRequest != nil {
		s.wakeOnRequest = *req.WakeOnRequest
	}
	if req.IdleTimeoutSeconds != nil {
		s.idleTimeoutSeconds = *req.IdleTimeoutSeconds
	}

	cfgPath := config.GetConfigPath(s.configPath)
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if req.WakeOnRequest != nil {
			cfg.WakeOnRequest = *req.WakeOnRequest
		}
		if req.IdleTimeoutSeconds != nil {
			cfg.IdleTimeoutSeconds = *req.IdleTimeoutSeconds
		}
		_ = config.SaveConfig(cfgPath, cfg)
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"wake_on_request":      s.wakeOnRequest,
		"idle_timeout_seconds": s.idleTimeoutSeconds,
	})
}
