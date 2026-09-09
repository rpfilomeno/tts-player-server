# AGENTS.md

Single-file Go app: `main.go` (~760 lines). System tray TTS player backed by Kokoro-FastAPI.

## Build & run

- `mise run install` or `go mod tidy` — install deps
- `mise run build` or `pwsh ./scripts/build.ps1` — build Windows binary (uses `-H windowsgui`)
- `go run main.go` — run (requires display; won't work headless)

## Prerequisites

- Kokoro-FastAPI must be running locally (default `http://localhost:49112/v1/audio/speech`).
- API address, model, voice, and maxChunkSize are configurable via `config.json` (falls back to hardcoded defaults if missing).
- Go 1.25 (per `mise.toml` and `go.mod`).

## Key facts

- **No tests.** No test files exist.
- **Windows-only.** Systray and `windowsgui` linker flag assume Windows.
- **No lint/typecheck configured.** No linter, formatter, or typecheck tooling is set up.
- **Single entrypoint:** `main.go` contains all logic — HTTP server (`:50059`), worker goroutine, audio playback via `beep`, systray UI.
- **Two HTTP endpoints:** `POST /queue` (text → TTS → play) and `POST /play` (raw audio bytes → play).
- **MP4 extraction hack:** `/play` tries MP3 decode first, then attempts MP4 mdat extraction — not a full MP4 parser.
