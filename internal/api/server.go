package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

type Server struct {
	db         *db.DB
	supervisor *supervisor.Supervisor
	staticFS   fs.FS
	mux        *http.ServeMux
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

func NewServer(database *db.DB, sup *supervisor.Supervisor, staticFS fs.FS) *Server {
	s := &Server{
		db:         database,
		supervisor: sup,
		staticFS:   staticFS,
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	return s
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

	// Web static files
	if s.staticFS != nil {
		fileServer := http.FileServer(http.FS(s.staticFS))
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
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
			evPayload, _ := json.Marshal(map[string]interface{}{
				"type":  "event",
				"event": ev,
			})
			fmt.Fprintf(w, "data: %s\n\n", evPayload)
			flusher.Flush()
			sendTelemetry()
		case <-ticker.C:
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
			} else {
				idleTickCount++
				if idleTickCount >= 4 { // 4 * 250ms = 1.0s in idle
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
