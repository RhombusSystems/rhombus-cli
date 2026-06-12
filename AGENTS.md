# AGENTS.md — Rhombus CLI

Guidance for AI coding agents working in this repository or using the `rhombus` CLI to operate the [Rhombus](https://www.rhombus.com) physical-security platform.

## What this is

`rhombus` is the official Go CLI for the Rhombus public API: cameras, access control, sensors, alerts, footage, and ~60 auto-generated API service groups, plus hand-written commands for login, footage review, video stitching, and AI chat. Platform overview for agents: <https://www.rhombus.com/llms.txt> · API auth guide: <https://www.rhombus.com/auth.md>.

## Using the CLI as an agent

- **Auth**: set `RHOMBUS_API_KEY` (create a key at <https://console.rhombus.com/settings/api-management>), or run `rhombus login` (interactive browser OAuth) / `rhombus configure` for stored profiles. `RHOMBUS_PROFILE` selects between profiles.
- **Discoverability**: `rhombus --help` lists every command group; `rhombus <group> --help` lists operations. Generated command names are kebab-case versions of API operations (e.g. `rhombus camera get-minimal-camera-state-list`).
- **Output**: commands emit JSON suitable for piping; prefer exact field extraction over screen-scraping.
- **The API itself**: base URL `https://api2.rhombussystems.com/api`, all endpoints are POST with JSON bodies, headers `x-auth-scheme: api-token` + `x-auth-apikey`. Full OpenAPI 3.0 spec: <https://api2.rhombussystems.com/api/openapi/public.json> (checked into this repo as `openapi.json`).

## Working on this repository

- **Build**: `make build` (runs codegen then `go build`). Requires Go 1.26+.
- **Codegen**: `cmd/generated/*.go` is generated from `openapi.json` by `./codegen` — never edit generated files by hand; change the generator or the spec and run `make generate`.
- **Hand-written commands** live under `cmd/` (login, configure, footage, alert, context, analyze, stitch, chat, voice) and `internal/`.
- **Install locally**: `make install` (copies to `/usr/local/bin/rhombus`).
- Keep changes consistent with existing command UX: kebab-case flags, JSON output by default.

## Related resources

- Developer docs: <https://api-docs.rhombus.community/> (LLM index: <https://api-docs.rhombus.community/llms.txt>)
- MCP server: <https://github.com/RhombusSystems/rhombus-node-mcp>
- Python examples: <https://github.com/RhombusSystems/api-examples-python>
- Community: <https://rhombus.community/>
