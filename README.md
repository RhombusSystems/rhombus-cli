# Rhombus CLI

Command-line interface for the [Rhombus](https://www.rhombus.com) physical security platform API. Provides direct terminal access to cameras, access control, alerts, sensors, and 60+ API resource categories — plus higher-level commands for video stitching, frame analysis, real-time monitoring, and AI-powered chat.

## Installation

### Homebrew (macOS/Linux)

```sh
brew install RhombusSystems/tap/rhombus
```

### Shell script (macOS/Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/RhombusSystems/rhombus-cli/main/install.sh | sh
```

On Linux, this automatically uses `.deb` or `.rpm` packages when a compatible package manager is detected.

### PowerShell (Windows)

```powershell
irm https://raw.githubusercontent.com/RhombusSystems/rhombus-cli/main/install.ps1 | iex
```

### From source

Requires Go 1.26+.

```sh
git clone https://github.com/RhombusSystems/rhombus-cli.git
cd rhombus-cli
make install
```

## Authentication

### Browser login (recommended)

```sh
rhombus login
```

Opens your browser for OAuth2 authentication, then creates and stores an API key locally. Supports both certificate-based (mTLS) and token-based auth.

### Manual configuration

```sh
rhombus configure
```

Prompts for an API key, output format, and endpoint URL. Useful when you already have an API key.

### Environment variables

| Variable | Description |
|---|---|
| `RHOMBUS_API_KEY` | API key (overrides credentials file) |
| `RHOMBUS_PROFILE` | Profile name (default: `default`) |
| `RHOMBUS_OUTPUT` | Output format: `json`, `table`, `text` |
| `RHOMBUS_ENDPOINT_URL` | API endpoint override |

### Profiles

Manage multiple accounts or environments with named profiles:

```sh
rhombus login --profile staging
rhombus camera get-minimal-camera-state-list --profile staging
```

Credentials are stored in `~/.rhombus/credentials` (file permissions 600) and config in `~/.rhombus/config`.

## Usage

```
rhombus <command> [subcommand] [flags]
```

### Global flags

```
--profile string    Configuration profile (default: "default")
--output string     Output format: json, table, text (default: json)
--api-key string    Override API key
--endpoint-url string  Override API endpoint
--partner-org string   Client org name or UUID (partner accounts)
```

## Commands

### Generated API commands

62 resource commands are auto-generated from the Rhombus OpenAPI spec, covering the full API surface. Examples:

```sh
# List all cameras
rhombus camera get-minimal-camera-state-list

# Get camera details
rhombus camera get-full-camera-state --camera-uuid <uuid>

# List recent alerts
rhombus event get-policy-alerts-v2

# Get alert details with bounding boxes
rhombus event get-policy-alert-details --alert-uuid <uuid>

# Manage locations
rhombus location get-locations

# Door access control
rhombus door-controller grant-access --door-uuid <uuid>
```

Every generated command supports:

- `--generate-cli-skeleton` — prints a JSON template of all accepted parameters
- `--cli-input-json '<json>'` or `--cli-input-json file://params.json` — pass complex request bodies as JSON

**Resource categories:** access-control, alert-monitoring, audio-gateway, badge-reader, camera, climate, door, door-controller, doorbell-camera, elevator, event, event-search, export, face-recognition-event, face-recognition-person, feature, integrations, location, lockdown-plan, logistics, oauth, occupancy, org, partner, permission, policy, report, rules, scene-query, schedule, search, sensor, user, vehicle, video, webhook-integrations, and more.

### Alert management

```sh
# Recent alerts (optionally filtered by camera)
rhombus alert recent --camera "Front Lobby" --max 10

# Download alert thumbnail
rhombus alert thumb <alert-uuid> --output thumb.jpg

# Download alert video clip
rhombus alert download <alert-uuid> --output clip.mp4

# Open alert clip in browser
rhombus alert play <alert-uuid>
```

### Live footage

```sh
# Open live view for a camera
rhombus footage "Front Lobby"

# Jump to a specific time
rhombus footage "Front Lobby" --start "5m ago"
rhombus footage "Front Lobby" --start 1711900800000
```

Starts a local HTTP server with an authenticated player and opens your browser.

### Real-time alert monitoring

```sh
# Stream policy alerts as they fire
rhombus monitor

# Include all event types
rhombus monitor --all-events

# JSON output for piping
rhombus monitor --json
```

Connects via WebSocket (STOMP protocol) with automatic reconnection.

### Video stitching

Requires `ffmpeg`.

```sh
# Stitch events across cameras at a location
rhombus stitch --location "HQ" --start "2h ago" --end "1h ago"

# Specific cameras with buffer
rhombus stitch --camera "Entrance" --camera "Lobby" --start "30m ago" --buffer 5
```

Creates a grid-layout MP4 with timestamp overlays from events across multiple cameras.

### Frame analysis

```sh
# Analyze alert frames
rhombus analyze alert <alert-uuid>

# Analyze footage across cameras at a location
rhombus analyze footage --location "HQ" --start "1h ago" --end "30m ago"

# Include motion metadata, output raw frames
rhombus analyze footage "Lobby Cam" --start "2h ago" --include-motion --raw
```

Extracts intelligently-sampled frames with activity detection metadata. Outputs a manifest suitable for external ML pipelines.

### Deployment context

```sh
# Generate full snapshot of all locations and cameras with stills
rhombus context generate

# Details for a specific location or camera
rhombus context location "HQ"
rhombus context camera "Front Lobby"
```

Produces a structured manifest of your deployment — locations, cameras, hardware info, coordinates, and current stills.

### AI chat (Rhombus MIND)

```sh
# Interactive chat
rhombus chat

# Voice-powered chat (requires sox and whisper-cpp)
rhombus voice --model base
```

Natural language interface to your Rhombus deployment. The chat agent can execute CLI commands on your behalf.

### Partner accounts

For partner/multi-tenant organizations, pass `--partner-org` to operate on a client org:

```sh
rhombus camera get-minimal-camera-state-list --partner-org "Acme Corp"
rhombus camera get-minimal-camera-state-list --partner-org <org-uuid>
```

Name matching is case-insensitive and substring-based. If multiple orgs match, you'll be prompted to select one.

## Configuration files

| Path | Purpose |
|---|---|
| `~/.rhombus/config` | Default output format, endpoint URL (INI) |
| `~/.rhombus/credentials` | API keys and cert paths per profile (INI, 600 perms) |
| `~/.rhombus/certs/<profile>/` | Client certificates and private keys |

## Configuration precedence

1. CLI flags
2. Environment variables
3. Profile config files
4. Defaults

## Platforms

| OS | Architectures |
|---|---|
| macOS | amd64, arm64 |
| Linux | amd64, arm64 |
| Windows | amd64, arm64 |

## License

MIT
