# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- Orphan-safe process management: `llama-server` processes survive daemon restarts
- `ReattachRunning()`: daemon re-adopts running models on startup via PID + healthcheck
- CLI flag `-config <path>` for specifying config file path
- CLI flag `-n <lines>` for `logs` command
- Environment variable overrides: `TELEGRAM_ADMIN_ID`, `HTTP_PORT`, `DB_PATH`, `LOG_DIR`
- GitHub Actions CI workflow (`go test`, `go vet`, `go build`)

### Fixed
- CLI `-config` flag was documented but never parsed — now works correctly
- CLI `logs -n` flag was documented but hardcoded to 50 — now parsed
- Background goroutine race condition in DB dead PID cleanup
- REST API documentation mismatches (endpoint names, MCP tool prefixes)
- SPEC.md endpoint for favorites now matches actual implementation

### Changed
- Child `llama-server` processes now run in their own session (`Setsid: true`)
- Dead PID cleanup in DB layer is now synchronous instead of spawning uncoordinated goroutines
