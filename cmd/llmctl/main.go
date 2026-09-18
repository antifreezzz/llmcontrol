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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/antifreezzz/llmcontrol/internal/api"
	"github.com/antifreezzz/llmcontrol/internal/config"
	"github.com/antifreezzz/llmcontrol/internal/db"
	"github.com/antifreezzz/llmcontrol/internal/downloader"
	"github.com/antifreezzz/llmcontrol/internal/importer"
	"github.com/antifreezzz/llmcontrol/internal/mcp"
	"github.com/antifreezzz/llmcontrol/internal/stt"
	"github.com/antifreezzz/llmcontrol/internal/supervisor"
	"github.com/antifreezzz/llmcontrol/internal/telegram"
	"github.com/antifreezzz/llmcontrol/internal/tunnel"
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
		runDaemon(cfg, database, sup, configPath)

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

	case "pull", "download":
		if len(filteredArgs) < 2 {
			fmt.Println("Usage: llmctl pull <repo_id> [quant_or_filename]")
			fmt.Println("Example: llmctl pull LiquidAI/LFM2.5-2.6B-GGUF Q4_K_M")
			os.Exit(1)
		}
		repoID := filteredArgs[1]
		quantOrFile := ""
		if len(filteredArgs) >= 3 {
			quantOrFile = filteredArgs[2]
		}
		dlMgr := downloader.NewManager(cfg.ModelsDir, database)
		fmt.Printf("🔍 Inspecting Hugging Face repository: %s ...\n", repoID)
		info, err := dlMgr.InspectRepo(ctx, repoID)
		if err != nil {
			fmt.Printf("❌ Inspection failed: %v\n", err)
			os.Exit(1)
		}
		if len(info.Files) == 0 {
			fmt.Println("❌ No GGUF files found in repository.")
			os.Exit(1)
		}

		var chosenFile *downloader.HuggingFaceFile
		if quantOrFile != "" {
			for _, f := range info.Files {
				if strings.EqualFold(f.Quant, quantOrFile) || strings.EqualFold(f.Filename, quantOrFile) || strings.Contains(strings.ToLower(f.Filename), strings.ToLower(quantOrFile)) {
					chosenFile = &f
					break
				}
			}
		}
		if chosenFile == nil {
			for _, f := range info.Files {
				if !f.IsMMProj && !f.IsMTP {
					if strings.Contains(f.Quant, "Q4_K_M") || strings.Contains(f.Quant, "Q4_0") || strings.Contains(f.Quant, "Q8_0") {
						chosenFile = &f
						break
					}
				}
			}
			if chosenFile == nil && len(info.Files) > 0 {
				chosenFile = &info.Files[0]
			}
		}

		fmt.Printf("📥 File: %s (%s, %s)\n", chosenFile.Filename, chosenFile.Quant, chosenFile.SizeStr)
		fmt.Printf("📁 Path: %s\n", filepath.Join(dlMgr.GetModelsDir(), repoID, chosenFile.Filename))

		job, err := dlMgr.StartDownload(repoID, chosenFile.Filename, true, "")
		if err != nil {
			fmt.Printf("❌ Failed to start download: %v\n", err)
			os.Exit(1)
		}

		for {
			time.Sleep(400 * time.Millisecond)
			if job.Status == downloader.StatusCompleted {
				fmt.Printf("\r\033[K✅ Download complete! [100.0%%] %s (%s)\n", chosenFile.Filename, downloader.FormatBytes(job.TotalBytes))
				fmt.Println("🎉 Model automatically registered in database with standard inference profiles.")
				break
			}
			if job.Status == downloader.StatusFailed {
				fmt.Printf("\r\033[K❌ Download failed: %s\n", job.Error)
				os.Exit(1)
			}
			if job.Status == downloader.StatusCancelled {
				fmt.Printf("\r\033[K⚠️ Download cancelled.\n")
				os.Exit(1)
			}
			fmt.Printf("\r\033[K⏳ Downloading: %5.1f%% | %s / %s | %s",
				job.ProgressPct,
				downloader.FormatBytes(job.DownloadedBytes),
				downloader.FormatBytes(job.TotalBytes),
				job.SpeedStr,
			)
		}

	case "downloads":
		dlMgr := downloader.NewManager(cfg.ModelsDir, database)
		jobs := dlMgr.ListJobs()
		if len(jobs) == 0 {
			fmt.Println("No active or recent downloads.")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "STATUS\tREPO\tFILE\tPROGRESS\tSPEED")
		for _, j := range jobs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%s\n", j.Status, j.RepoID, j.Filename, j.ProgressPct, j.SpeedStr)
		}
		w.Flush()

	case "tunnel-server":
		ctlListen := ":8443"
		bindListen := "127.0.0.1:8666"
		token := cfg.VPSToken

		for i := 1; i < len(filteredArgs); i++ {
			if (filteredArgs[i] == "--listen" || filteredArgs[i] == "-l") && i+1 < len(filteredArgs) {
				ctlListen = filteredArgs[i+1]
				i++
			} else if (filteredArgs[i] == "--bind" || filteredArgs[i] == "-b") && i+1 < len(filteredArgs) {
				bindListen = filteredArgs[i+1]
				i++
			} else if (filteredArgs[i] == "--token" || filteredArgs[i] == "-t") && i+1 < len(filteredArgs) {
				token = filteredArgs[i+1]
				i++
			}
		}

		if token == "" {
			token = os.Getenv("VPS_TOKEN")
		}

		fmt.Println("☁️ Starting LLMControl Tunnel Server on VPS...")
		fmt.Printf("🔒 Control Port: %s\n", ctlListen)
		fmt.Printf("🌐 Local Bind:   %s\n", bindListen)
		if token != "" {
			fmt.Println("🔑 Auth Token:   Configured")
		} else {
			fmt.Println("⚠️ Auth Token:   NOT configured (open access)")
		}

		srv := tunnel.NewServer(tunnel.ServerConfig{
			ControlListen: ctlListen,
			BindListen:    bindListen,
			Token:         token,
		})
		defer srv.Close()

		stopChan := make(chan os.Signal, 1)
		signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stopChan
			fmt.Println("\n🛑 Stopping tunnel server...")
			srv.Close()
			os.Exit(0)
		}()

		if err := srv.Start(); err != nil {
			fmt.Printf("Tunnel server error: %v\n", err)
			os.Exit(1)
		}

	default:
		printUsage()
	}
}

func runDaemon(cfg config.Config, database *db.DB, sup *supervisor.Supervisor, configPath string) {
	fmt.Println("🚀 Starting LLM Control Center Daemon...")
	fmt.Printf("📁 Database: %s\n", cfg.DBPath)
	fmt.Printf("📁 Models Dir: %s\n", cfg.ModelsDir)
	fmt.Printf("🌐 Web & REST API: http://%s:%d\n", cfg.HTTPHost, cfg.HTTPPort)

	// Re-attach to any llama-server processes that survived a previous daemon shutdown
	sup.ReattachRunning(context.Background())

	dlMgr := downloader.NewManager(cfg.ModelsDir, database)
	srv := api.NewServer(database, sup, dlMgr, web.StaticFS())
	srv.SetConfigPath(configPath)

	// Initialize Whisper STT
	sttSvc := stt.NewService(stt.Config{
		BinaryPath: cfg.WhisperBinaryPath,
		ModelPath:  cfg.WhisperModelPath,
	})
	srv.SetSTTService(sttSvc)

	// Initialize Reverse Tunnel Client
	tunnelMgr := tunnel.NewClientManager(
		fmt.Sprintf("127.0.0.1:%d", cfg.HTTPPort),
		cfg.VPSHost,
		cfg.VPSTunnelPort,
		cfg.VPSRemotePort,
		cfg.VPSToken,
	)
	if cfg.VPSTargetModel != "" {
		tunnelMgr.SetTargetModel(cfg.VPSTargetModel, "default")
	}
	srv.SetTunnelManager(tunnelMgr)

	if cfg.VPSHost != "" {
		targetModel := cfg.VPSTargetModel
		if err := tunnelMgr.Start(targetModel, "default"); err != nil {
			fmt.Printf("⚠️ Failed to auto-start tunnel: %v\n", err)
		} else {
			fmt.Printf("☁️ Reverse tunnel auto-started to %s:%d (remote: %d, target: %s)\n", cfg.VPSHost, cfg.VPSTunnelPort, cfg.VPSRemotePort, targetModel)
		}
	}

	// Configure On-Demand Wake and Idle Auto-Stop
	srv.SetOnDemandConfig(cfg.WakeOnRequest, cfg.IdleTimeoutSeconds)

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
	srv.Close()
	tunnelMgr.Stop()
	fmt.Println("👋 Goodbye!")
}

func printUsage() {
	fmt.Print(`⚡ LLM Control Center (llmctl)

Commands:
  daemon               Run background daemon (REST API, Web UI, Telegram Bot, Tunnel Client)
  tunnel-server        Run reverse tunnel server on VPS (--listen :8443 --bind 127.0.0.1:8666 --token <secret>)
  import [dir]         Import existing shell scripts (default: ~/bin) into SQLite
  list, ps             List all models, runtime statuses, memory & speeds
  start <model>        Start a model (optional: --profile <name>)
  stop <model|--all>   Stop a running model or all models
  pull <repo> [quant]  Download GGUF model from Hugging Face & auto-register
  bench <model>        Run benchmark and calculate tok/s
  logs <model>         Show last log lines for a model (optional: -n <lines>)
  mcp                  Start Model Context Protocol (MCP) stdio server for AI assistants

Global flags:
  -config <path>       Path to config.json (default: ./config.json or ~/.llmcontrol/config.json)
`)
}
