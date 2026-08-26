package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/api"
	"github.com/antifreezzz/llmcontrol/internal/config"
	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/importer"
	"github.com/antifreezzz/llmcontrol/internal/mcp"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
	"github.com/antifreezzz/llmcontrol/internal/telegram"
	"github.com/antifreezzz/llmcontrol/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	// Scan for global flags before the command
	configPath := ""
	var filteredArgs []string
	for i := 1; i < len(os.Args); i++ {
		if (os.Args[i] == "-config" || os.Args[i] == "--config") && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
			i++ // skip the value
		} else {
			filteredArgs = append(filteredArgs, os.Args[i])
		}
	}

	if len(filteredArgs) == 0 {
		printUsage()
		return
	}

	command := filteredArgs[0]

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Printf("Warning: error loading config: %v\n", err)
	}

	database, err := db.NewDB(cfg.DBPath)
	if err != nil {
		fmt.Printf("Fatal: failed to initialize database at %s: %v\n", cfg.DBPath, err)
		os.Exit(1)
	}
	defer database.Close()

	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		DB:            database,
		LogDir:        cfg.LogDir,
		Host:          "127.0.0.1",
		ExclusiveMode: cfg.ExclusiveMode,
	})

	ctx := context.Background()

	switch command {
	case "daemon", "server", "run":
		runDaemon(cfg, database, sup)

	case "mcp":
		if err := mcp.RunStdio(database, sup); err != nil {
			fmt.Printf("MCP error: %v\n", err)
		}

	case "import":
		dir := filepath.Join(os.Getenv("HOME"), "bin")
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		fmt.Printf("📦 Importing legacy model scripts from: %s\n", dir)
		res, err := importer.ImportDirectory(ctx, database, dir)
		if err != nil {
			fmt.Printf("Import failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Done! Parsed %d scripts, successfully imported %d models into SQLite.\n", res.TotalScripts, res.ImportedModels)
		if len(res.Errors) > 0 {
			fmt.Println("⚠️ Warnings/Errors during import:")
			for _, e := range res.Errors {
				fmt.Printf("  • %s\n", e)
			}
		}

	case "list", "ps", "ls":
		models, err := database.ListModels(ctx)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "STATUS\tMODEL ID\tNAME\tPORT\tPID\tCTX\tFAV\tSPEED")
		for _, m := range models {
			status := "🔴 STOPPED"
			pidStr := "-"
			ctxStr := fmt.Sprintf("%s", m.DefaultProfile)
			if m.Runtime != nil {
				if m.Runtime.Status == "running" {
					status = "🟢 RUNNING"
					pidStr = strconv.Itoa(m.Runtime.PID)
					ctxStr = m.Runtime.ProfileName
				} else if m.Runtime.Status == "starting" {
					status = "🟡 STARTING"
					pidStr = strconv.Itoa(m.Runtime.PID)
				}
			}

			favStr := ""
			if m.IsFavorite {
				favStr = "⭐"
			}

			speedStr := "-"
			bench, _ := database.GetLatestBenchmark(ctx, m.ID)
			if bench != nil {
				speedStr = fmt.Sprintf("%.1f t/s", bench.PredictedPerSecond)
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", status, m.ID, m.Name, m.DefaultPort, pidStr, ctxStr, favStr, speedStr)
		}
		w.Flush()

		sysStat := supervisor.GetSystemStatus()
		fmt.Println()
		fmt.Println(sysStat.FormatSummary())

	case "start":
		if len(os.Args) < 3 {
			fmt.Println("Usage: llmctl start <model_id> [--profile <profile>] [--lan | --local]")
			os.Exit(1)
		}
		modelID := os.Args[2]
		profile := ""
		var allowLAN *bool
		for i := 3; i < len(os.Args); i++ {
			if os.Args[i] == "--profile" && i+1 < len(os.Args) {
				profile = os.Args[i+1]
				i++
			} else if os.Args[i] == "--lan" {
				val := true
				allowLAN = &val
			} else if os.Args[i] == "--local" {
				val := false
				allowLAN = &val
			}
		}
		lanStr := "auto"
		if allowLAN != nil {
			if *allowLAN {
				lanStr = "LAN (0.0.0.0)"
			} else {
				lanStr = "Local (127.0.0.1)"
			}
		}
		fmt.Printf("🚀 Starting model '%s' (profile: %s, network: %s)...\n", modelID, profile, lanStr)

		// Try daemon API first if running
		daemonURL := fmt.Sprintf("http://127.0.0.1:%d/api/models/%s/start", cfg.HTTPPort, modelID)
		bodyData := map[string]interface{}{"profile": profile}
		if allowLAN != nil {
			bodyData["allow_lan"] = *allowLAN
		}
		body, _ := json.Marshal(bodyData)
		resp, err := http.Post(daemonURL, "application/json", bytes.NewReader(body))
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			fmt.Println("✅ Started via daemon! Waiting for healthcheck & benchmark...")
			return
		}
		if resp != nil {
			resp.Body.Close()
		}

		var lanOverride []bool
		if allowLAN != nil {
			lanOverride = []bool{*allowLAN}
		}
		if err := sup.StartModel(ctx, modelID, profile, lanOverride...); err != nil {
			fmt.Printf("Failed to start: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✅ Started! Waiting for healthcheck & benchmark...")

	case "stop":
		if len(os.Args) < 3 {
			fmt.Println("Usage: llmctl stop <model_id | --all>")
			os.Exit(1)
		}
		target := os.Args[2]
		if target == "--all" || target == "all" {
			daemonURL := fmt.Sprintf("http://127.0.0.1:%d/api/models/stop-all", cfg.HTTPPort)
			resp, err := http.Post(daemonURL, "application/json", nil)
			if err == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				fmt.Println("⏹ All models stopped.")
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
			if err := sup.StopAll(ctx); err != nil {
				fmt.Printf("Failed to stop all: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("⏹ All models stopped.")
		} else {
			daemonURL := fmt.Sprintf("http://127.0.0.1:%d/api/models/%s/stop", cfg.HTTPPort, target)
			resp, err := http.Post(daemonURL, "application/json", nil)
			if err == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				fmt.Printf("⏹ Stopped model '%s'.\n", target)
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
			if err := sup.StopModel(ctx, target); err != nil {
				fmt.Printf("Failed to stop model '%s': %v\n", target, err)
				os.Exit(1)
			}
			fmt.Printf("⏹ Stopped model '%s'.\n", target)
		}

	case "bench":
		if len(os.Args) < 3 {
			fmt.Println("Usage: llmctl bench <model_id>")
			os.Exit(1)
		}
		modelID := os.Args[2]
		fmt.Printf("🧪 Running benchmark on '%s'...\n", modelID)
		bench, err := sup.RunBenchmark(ctx, modelID)
		if err != nil {
			fmt.Printf("Benchmark error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Prompt: %.1f tok/s | Generation: %.1f tok/s | Sample: '%s'\n", bench.PromptPerSecond, bench.PredictedPerSecond, bench.OutputSample)

	case "logs":
		if len(os.Args) < 3 {
			fmt.Println("Usage: llmctl logs <model_id> [-n <lines>]")
			os.Exit(1)
		}
		modelID := os.Args[2]
		lines := 50
		for i := 3; i < len(os.Args); i++ {
			if (os.Args[i] == "-n" || os.Args[i] == "--lines") && i+1 < len(os.Args) {
				if n, err := strconv.Atoi(os.Args[i+1]); err == nil && n > 0 {
					lines = n
				}
				i++
			}
		}
		logs, err := sup.GetLogs(modelID, lines)
		if err != nil {
			fmt.Printf("Error reading logs: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(logs)

	default:
		printUsage()
	}
}

func runDaemon(cfg config.Config, database *db.DB, sup *supervisor.Supervisor) {
	fmt.Println("🚀 Starting LLM Control Center Daemon...")
	fmt.Printf("📁 Database: %s\n", cfg.DBPath)
	fmt.Printf("🌐 Web & REST API: http://%s:%d\n", cfg.HTTPHost, cfg.HTTPPort)

	// Re-attach to any llama-server processes that survived a previous daemon shutdown
	sup.ReattachRunning(context.Background())

	srv := api.NewServer(database, sup, web.StaticFS())
	httpServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.HTTPHost, cfg.HTTPPort),
		Handler: srv.Router(),
	}

	// Start HTTP Server
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("HTTP Server error: %v\n", err)
		}
	}()

	// Start Telegram Bot if Token is provided
	if cfg.TelegramToken != "" {
		fmt.Printf("🤖 Starting Telegram Bot (Admin ID: %d)...\n", cfg.TelegramAdminID)
		bot := telegram.NewBot(telegram.BotConfig{
			Token:   cfg.TelegramToken,
			AdminID: cfg.TelegramAdminID,
			DB:      database,
			Sup:     sup,
		})
		go bot.StartPolling(context.Background())
	} else {
		fmt.Println("ℹ️ Telegram bot token not configured in config.json. Bot disabled.")
	}

	// Graceful shutdown on SIGINT/SIGTERM
	// NOTE: running llama-server processes are NOT stopped — they survive daemon restarts.
	// Use 'llmctl stop --all' to explicitly stop all models.
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
	<-stopChan

	fmt.Println("\n🛑 Shutting down daemon (models keep running)...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	fmt.Println("👋 Goodbye!")
}

func printUsage() {
	fmt.Print(`⚡ LLM Control Center (llmctl)

Commands:
  daemon               Run background daemon (REST API, Web UI, Telegram Bot)
  import [dir]         Import existing shell scripts (default: ~/bin) into SQLite
  list, ps             List all models, runtime statuses, memory & speeds
  start <model>        Start a model (optional: --profile <name>)
  stop <model|--all>   Stop a running model or all models
  bench <model>        Run benchmark and calculate tok/s
  logs <model>         Show last log lines for a model (optional: -n <lines>)
  mcp                  Start Model Context Protocol (MCP) stdio server for AI assistants

Global flags:
  -config <path>       Path to config.json (default: ./config.json or ~/.llmcontrol/config.json)
`)
}
