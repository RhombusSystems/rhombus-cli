---
name: rhombus-cli
description: Operate the Rhombus physical-security platform (cameras, access control, sensors, alerts, footage) from the terminal using the official rhombus CLI. Use when a task involves checking camera or device status, pulling alerts or events, reviewing or stitching footage, managing access control, or automating anything in a Rhombus deployment.
---

# Rhombus CLI

Official CLI for the [Rhombus](https://www.rhombus.com) public API — ~60 service groups covering cameras, access control, sensors, alerts, footage, locations, users, and webhooks.

## Setup

1. Install: `brew install RhombusSystems/tap/rhombus` (or see [README](https://github.com/RhombusSystems/rhombus-cli) for shell/PowerShell installers).
2. Authenticate, either:
   - `rhombus login` — interactive browser OAuth, stores a key locally; or
   - set `RHOMBUS_API_KEY` — create a key in the [Rhombus Console](https://console.rhombus.com/settings/api-management).

## Usage patterns

- Discover commands: `rhombus --help`, then `rhombus <group> --help`.
- Command names are kebab-case API operations: `rhombus camera get-minimal-camera-state-list`, `rhombus alert get-recent-alerts`, `rhombus door get-door-state-list`.
- Output is JSON — pipe to `jq` for extraction.
- Common flows:
  - Device health: `rhombus camera get-minimal-camera-state-list`
  - Recent alerts: `rhombus alert` group
  - Footage review: `rhombus analyze` (AI analysis of a camera + time window), `rhombus stitch` (multi-camera clip assembly)
  - Natural-language queries: `rhombus chat`

## References

- API auth guide: <https://www.rhombus.com/auth.md>
- OpenAPI spec: <https://api2.rhombussystems.com/api/openapi/public.json>
- Developer docs: <https://api-docs.rhombus.community/>
