# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

```bash
make generate    # Parse openapi.json → cmd/generated/ (required before build)
make build       # Run generate + compile to ./rhombus binary
make install     # Copy binary to /usr/local/bin
make clean       # Remove binary AND wipe cmd/generated/*.go — re-run `make generate` before next build
```

Requires Go 1.26.1+ (see `go.mod`). Version is injected via ldflags (`-X main.version={{.Version}}`); defaults to "dev" in main.go.

No test suite exists. CGO is disabled (`CGO_ENABLED=0`).

`openapi.json` (~4 MB) is checked in at the repo root and is the source of truth for generated commands. Edits to it require `make generate` to propagate into `cmd/generated/`.

### Runtime external dependencies

Some manual commands shell out to native binaries — they must be on `PATH` to test those flows:

- `stitch` — requires `ffmpeg`
- `voice` — requires `sox` and `whisper-cpp`

## Architecture

This is a CLI for the Rhombus physical security platform API, built with Cobra. Entry point is `main.go` → `cmd.Execute()`.

### Two kinds of commands

1. **Auto-generated API commands** (`cmd/generated/`): 62 service files generated from `openapi.json` by the codegen tool (`go run ./codegen/ <spec> <outdir>`). Every operation follows the same pattern: load config → collect flags → merge `--cli-input-json` → POST to `/api/<resource>/<operation>` → format output. Do not hand-edit these files.

2. **Manual commands** (`cmd/`): Complex features that need custom UX — alert management, live footage, WebSocket monitoring, video stitching (ffmpeg), frame analysis, deployment context, AI chat/voice. File map: `alert.go`, `footage.go`, `monitor.go` + `monitor_actions.go`, `stitch.go`, `analyze.go`, `context.go`, `chat.go`, `voice.go`, `configure.go`, `login.go`, plus `root.go` (persistent flags + command registration).

### Internal packages

- **`internal/client`** — HTTP client with auth handling (API key, mTLS, partner tokens). All API calls go through `APICall(cfg, path, body)`. Verbose logging via `--verbose` flag.
- **`internal/config`** — Config loading with precedence: CLI flags > env vars (`RHOMBUS_API_KEY`, `RHOMBUS_PROFILE`, `RHOMBUS_OUTPUT`, `RHOMBUS_ENDPOINT_URL`) > profile INI files (`~/.rhombus/config`, `~/.rhombus/credentials`) > defaults.
- **`internal/output`** — Output formatting. JSON is implemented; table/text are Phase 2 stubs.
- **`internal/params`** — Flag collection, JSON body building, `--cli-input-json` / `--generate-cli-skeleton` support. Converts kebab-case flags to camelCase for the API.

### Code generation pipeline

`codegen/` has four files: `main.go` (entry), `parser.go` (OpenAPI → service structs), `naming.go` (naming conventions), `writer.go` (Go template rendering + gofmt). Output goes to `cmd/generated/` including `register.go` which wires all services into the root command.

### Key patterns

- Global persistent flags on root: `--profile`, `--output`, `--api-key`, `--endpoint-url`, `--partner-org`, `--verbose`. These are excluded from API request bodies via `CollectFlags`.
- Partner/multi-tenant: `--partner-org` accepts name or UUID; PersistentPreRunE resolves names to UUIDs via the partner API.
- Auth modes: token (`x-auth-apikey` header), mTLS (client cert/key with custom PEM parsing for negative serial numbers), OAuth2 browser login.
- WebSocket monitoring uses STOMP 1.2 protocol with heartbeats and reconnection.

### Release

Goreleaser v2 builds for linux/darwin/windows × amd64/arm64. Tags (`v*`) trigger `.github/workflows/release.yml` which publishes to GitHub releases + Homebrew tap.
