package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
)

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Server struct {
	db         *db.DB
	supervisor *supervisor.Supervisor
}

func NewServer(database *db.DB, sup *supervisor.Supervisor) *Server {
	return &Server{
		db:         database,
		supervisor: sup,
	}
}

func (s *Server) ListTools() []Tool {
	return []Tool{
		{
			Name:        "llm_list_models",
			Description: "List all configured models, their profiles, ports, and runtime states (running/stopped).",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "llm_get_status",
			Description: "Get current system status, memory (RAM & Intel Arc VRAM), and active model details.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "llm_start_model",
			Description: "Start a specified LLM by ID with an optional inference profile (e.g., 'default', 'fast', 'long', 'xlong').",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id": map[string]interface{}{
						"type":        "string",
						"description": "ID of the model to start (e.g. 'gemma4', 'cyber', 'ornith')",
					},
					"profile": map[string]interface{}{
						"type":        "string",
						"description": "Profile name (e.g. 'fast', 'default', 'long', 'max', 'api')",
					},
				},
				"required": []string{"model_id"},
			},
		},
		{
			Name:        "llm_stop_model",
			Description: "Stop a running model by ID.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id": map[string]interface{}{
						"type":        "string",
						"description": "ID of the model to stop",
					},
				},
				"required": []string{"model_id"},
			},
		},
		{
			Name:        "llm_stop_all",
			Description: "Stop all running LLM models to free up GPU and system resources.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "llm_benchmark",
			Description: "Run a live benchmark on an active model to calculate prompt and generation tok/s.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id": map[string]interface{}{
						"type":        "string",
						"description": "ID of the running model to benchmark",
					},
				},
				"required": []string{"model_id"},
			},
		},
		{
			Name:        "llm_get_logs",
			Description: "Get latest log lines of a model.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id": map[string]interface{}{
						"type": "string",
					},
					"lines": map[string]interface{}{
						"type": "integer",
					},
				},
				"required": []string{"model_id"},
			},
		},
		{
			Name:        "llm_save_model",
			Description: "Create or update a model configuration in llmcontrol.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":              map[string]interface{}{"type": "string", "description": "Unique model identifier (e.g. 'ornith', 'ornith-35b')"},
					"name":            map[string]interface{}{"type": "string", "description": "Display name of the model"},
					"engine_id":       map[string]interface{}{"type": "string", "description": "Engine ID ('llama-vk' or 'llama-sycl'), default: 'llama-vk'"},
					"model_path":      map[string]interface{}{"type": "string", "description": "Absolute path to model GGUF"},
					"mmproj_path":     map[string]interface{}{"type": "string", "description": "Absolute path to mmproj GGUF (Vision CLIP adapter, optional)"},
					"mtp_path":        map[string]interface{}{"type": "string", "description": "Absolute path to MTP draft GGUF (optional)"},
					"default_port":    map[string]interface{}{"type": "integer", "description": "Default server port (default 8080)"},
					"default_profile": map[string]interface{}{"type": "string", "description": "Default profile name (default 'default')"},
					"is_favorite":     map[string]interface{}{"type": "boolean", "description": "Whether model is marked as favorite"},
				},
				"required": []string{"id", "name", "model_path"},
			},
		},
		{
			Name:        "llm_delete_model",
			Description: "Delete a model configuration by ID.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id": map[string]interface{}{"type": "string", "description": "ID of the model to delete"},
				},
				"required": []string{"model_id"},
			},
		},
		{
			Name:        "llm_save_profile",
			Description: "Create or update a profile for a model.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id":    map[string]interface{}{"type": "string", "description": "Model ID"},
					"name":        map[string]interface{}{"type": "string", "description": "Profile name (e.g. 'default', 'fast', 'vision')"},
					"description": map[string]interface{}{"type": "string", "description": "Human description of profile settings"},
					"ctx_size":    map[string]interface{}{"type": "integer", "description": "Context size in tokens"},
					"parallel":    map[string]interface{}{"type": "integer", "description": "Number of parallel slots (default 1)"},
					"kv_type":     map[string]interface{}{"type": "string", "description": "KV cache quantization: 'q8_0', 'q4_0', 'f16'"},
					"flash_attn":        map[string]interface{}{"type": "string", "description": "Flash attention: 'on', 'off', 'auto'"},
					"use_mtp":           map[string]interface{}{"type": "boolean", "description": "Enable MTP speculative decoding if available"},
					"spec_type":         map[string]interface{}{"type": "string", "description": "Speculative decoding type: 'draft-dflash', 'draft-mtp', 'draft-simple', 'draft-eagle3', 'draft-dspark', 'none'"},
					"draft_model_path":  map[string]interface{}{"type": "string", "description": "Override path to draft/DFlash/MTP GGUF for this profile"},
					"draft_n_max":       map[string]interface{}{"type": "integer", "description": "Speculative draft max tokens (--spec-draft-n-max)"},
					"draft_ngl":         map[string]interface{}{"type": "integer", "description": "Speculative draft GPU layers offload (--spec-draft-ngl)"},
					"use_vision":        map[string]interface{}{"type": "boolean", "description": "Enable Vision mmproj projector"},
					"enable_ui":         map[string]interface{}{"type": "boolean", "description": "Enable llama-server WebUI"},
					"tools":             map[string]interface{}{"type": "string", "description": "Tools mode: 'safe', 'all', or empty"},
					"extra_args":        map[string]interface{}{"type": "string", "description": "Extra CLI flags to pass to llama-server"},
				},
				"required": []string{"model_id", "name", "ctx_size"},
			},
		},
		{
			Name:        "llm_delete_profile",
			Description: "Delete a profile of a model.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id":     map[string]interface{}{"type": "string", "description": "Model ID"},
					"profile_name": map[string]interface{}{"type": "string", "description": "Profile name"},
				},
				"required": []string{"model_id", "profile_name"},
			},
		},
		{
			Name:        "llm_set_favorite",
			Description: "Set or unset a model as favorite.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"model_id":    map[string]interface{}{"type": "string", "description": "Model ID"},
					"is_favorite": map[string]interface{}{"type": "boolean", "description": "Favorite flag"},
				},
				"required": []string{"model_id", "is_favorite"},
			},
		},
		{
			Name:        "llm_generate_image",
			Description: "Generate an image using a local diffusion model (sd-cli Vulkan) on Intel Arc GPU.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt":          map[string]interface{}{"type": "string", "description": "Text prompt describing the desired image"},
					"negative_prompt": map[string]interface{}{"type": "string", "description": "Negative prompt (things to avoid, optional)"},
					"model_id":        map[string]interface{}{"type": "string", "description": "Diffusion model ID (optional, defaults to first configured diffusion model)"},
					"width":           map[string]interface{}{"type": "integer", "description": "Width in pixels (e.g. 512, 768, 1024, default 512)"},
					"height":          map[string]interface{}{"type": "integer", "description": "Height in pixels (e.g. 512, 768, 1024, default 512)"},
					"steps":           map[string]interface{}{"type": "integer", "description": "Sampling steps (e.g. 4 for turbo, 20-30 for SDXL/Flux)"},
					"cfg_scale":       map[string]interface{}{"type": "number", "description": "Guidance / CFG scale (e.g. 1.0 for turbo, 7.0 for standard)"},
					"seed":            map[string]interface{}{"type": "integer", "description": "RNG seed (-1 for random)"},
					"clip_on_cpu":     map[string]interface{}{"type": "boolean", "description": "Keep text encoder on CPU to save GPU VRAM (recommended on 16GB Arc A770)"},
				},
				"required": []string{"prompt"},
			},
		},
	}
}

func (s *Server) CallTool(ctx context.Context, name string, argsRaw json.RawMessage) (*ToolResult, error) {
	switch name {
	case "llm_generate_image", "image_generate":
		var req supervisor.ImageGenRequest
		if err := json.Unmarshal(argsRaw, &req); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments: " + err.Error()}}}, nil
		}
		img, err := s.supervisor.GenerateImage(ctx, req)
		if err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "generation failed: " + err.Error()}}}, nil
		}
		resJSON, _ := json.MarshalIndent(img, "", "  ")
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Image generated successfully in %d ms!\nSaved to: %s\n\nMetadata:\n%s", img.DurationMs, img.FilePath, string(resJSON))}}}, nil

	case "llm_list_models":
		models, err := s.db.ListModels(ctx)
		if err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		b, _ := json.MarshalIndent(models, "", "  ")
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: string(b)}}}, nil

	case "llm_get_status":
		active, _ := s.db.GetActiveRuntimeStates(ctx)
		sysStat := supervisor.GetSystemStatus()
		res := map[string]interface{}{
			"active_models": active,
			"system":        sysStat,
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: string(b)}}}, nil

	case "llm_start_model":
		var args struct {
			ModelID string `json:"model_id"`
			Profile string `json:"profile"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments"}}}, nil
		}
		if err := s.supervisor.StartModel(ctx, args.ModelID, args.Profile); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Started model '%s' with profile '%s'", args.ModelID, args.Profile)}}}, nil

	case "llm_stop_model":
		var args struct {
			ModelID string `json:"model_id"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments"}}}, nil
		}
		if err := s.supervisor.StopModel(ctx, args.ModelID); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Stopped model '%s'", args.ModelID)}}}, nil

	case "llm_stop_all":
		if err := s.supervisor.StopAll(ctx); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: "All running models stopped."}}}, nil

	case "llm_benchmark":
		var args struct {
			ModelID string `json:"model_id"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments"}}}, nil
		}
		bench, err := s.supervisor.RunBenchmark(ctx, args.ModelID)
		if err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		b, _ := json.MarshalIndent(bench, "", "  ")
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: string(b)}}}, nil

	case "llm_get_logs":
		var args struct {
			ModelID string `json:"model_id"`
			Lines   int    `json:"lines"`
		}
		_ = json.Unmarshal(argsRaw, &args)
		logs, err := s.supervisor.GetLogs(args.ModelID, args.Lines)
		if err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: logs}}}, nil

	case "llm_save_model":
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
		if err := json.Unmarshal(argsRaw, &req); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments: " + err.Error()}}}, nil
		}
		existing, _ := s.db.GetModel(ctx, req.ID)
		if req.Name == "" && existing != nil {
			req.Name = existing.Name
		}
		if req.ModelPath == "" && existing != nil {
			req.ModelPath = existing.ModelPath
		}
		if req.ID == "" || req.Name == "" || req.ModelPath == "" {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "id, name, and model_path are required"}}}, nil
		}
		if req.EngineID == "" {
			if existing != nil && existing.EngineID != "" {
				req.EngineID = existing.EngineID
			} else {
				req.EngineID = "llama-vk"
			}
		}
		if req.DefaultPort <= 0 {
			if existing != nil && existing.DefaultPort > 0 {
				req.DefaultPort = existing.DefaultPort
			} else {
				req.DefaultPort = 8080
			}
		}
		if req.DefaultProfile == "" {
			if existing != nil && existing.DefaultProfile != "" {
				req.DefaultProfile = existing.DefaultProfile
			} else {
				req.DefaultProfile = "default"
			}
		}

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
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		if len(req.Profiles) > 0 {
			for _, p := range req.Profiles {
				p.ModelID = req.ID
				if p.CtxSize <= 0 {
					p.CtxSize = 4096
				}
				_ = s.db.SaveProfile(ctx, p)
			}
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Model '%s' saved successfully", m.ID)}}}, nil

	case "llm_delete_model":
		var args struct {
			ModelID string `json:"model_id"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil || args.ModelID == "" {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "model_id is required"}}}, nil
		}
		_ = s.supervisor.StopModel(ctx, args.ModelID)
		if err := s.db.DeleteModel(ctx, args.ModelID); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Model '%s' deleted successfully", args.ModelID)}}}, nil

	case "llm_save_profile":
		var p db.Profile
		if err := json.Unmarshal(argsRaw, &p); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "invalid arguments: " + err.Error()}}}, nil
		}
		if p.ModelID == "" || p.Name == "" {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "model_id and name are required"}}}, nil
		}
		if p.CtxSize <= 0 {
			p.CtxSize = 4096
		}
		if err := s.db.SaveProfile(ctx, p); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Profile '%s' for model '%s' saved successfully", p.Name, p.ModelID)}}}, nil

	case "llm_delete_profile":
		var args struct {
			ModelID     string `json:"model_id"`
			ProfileName string `json:"profile_name"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil || args.ModelID == "" || args.ProfileName == "" {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "model_id and profile_name are required"}}}, nil
		}
		if err := s.db.DeleteProfile(ctx, args.ModelID, args.ProfileName); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Profile '%s' for model '%s' deleted successfully", args.ProfileName, args.ModelID)}}}, nil

	case "llm_set_favorite":
		var args struct {
			ModelID    string `json:"model_id"`
			IsFavorite bool   `json:"is_favorite"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil || args.ModelID == "" {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "model_id is required"}}}, nil
		}
		if err := s.db.SetFavorite(ctx, args.ModelID, args.IsFavorite); err != nil {
			return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: err.Error()}}}, nil
		}
		return &ToolResult{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Favorite status for model '%s' updated to %v", args.ModelID, args.IsFavorite)}}}, nil

	default:
		return &ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Unknown tool: %s", name)}}}, nil
	}
}

func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		var resp JSONRPCResponse
		resp.JSONRPC = "2.0"
		resp.ID = req.ID

		switch req.Method {
		case "initialize":
			resp.Result = map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]string{
					"name":    "llmcontrol",
					"version": "1.0.0",
				},
				"capabilities": map[string]interface{}{
					"tools": map[string]bool{"listChanged": false},
				},
			}

		case "tools/list":
			resp.Result = map[string]interface{}{
				"tools": s.ListTools(),
			}

		case "tools/call":
			var callParams struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &callParams); err != nil {
				resp.Error = &RPCError{Code: -32602, Message: "Invalid params"}
			} else {
				res, err := s.CallTool(ctx, callParams.Name, callParams.Arguments)
				if err != nil {
					resp.Error = &RPCError{Code: -32000, Message: err.Error()}
				} else {
					resp.Result = res
				}
			}

		case "notifications/initialized":
			continue

		default:
			resp.Error = &RPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)}
		}

		b, _ := json.Marshal(resp)
		b = append(b, '\n')
		_, _ = out.Write(b)
	}
}

func RunStdio(database *db.DB, sup *supervisor.Supervisor) error {
	srv := NewServer(database, sup)
	return srv.ServeStdio(context.Background(), os.Stdin, os.Stdout)
}
