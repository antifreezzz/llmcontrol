package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

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
	Status      string                  `json:"status"` // 'running', 'idle'
	ActiveModel string                  `json:"active_model,omitempty"`
	Port        int                     `json:"port,omitempty"`
	Profile     string                  `json:"profile,omitempty"`
	TPS         float64                 `json:"tps,omitempty"`
	System      supervisor.SystemStatus `json:"system"`
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
	s.mux.HandleFunc("GET /api/models/{id}", s.handleGetModel)
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	}

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
		Profile string `json:"profile"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if err := s.supervisor.StartModel(r.Context(), id, req.Profile); err != nil {
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

	fmt.Fprintf(w, "data: connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
		}
	}
}
