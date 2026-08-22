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
	}
}

func (s *Server) CallTool(ctx context.Context, name string, argsRaw json.RawMessage) (*ToolResult, error) {
	switch name {
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
