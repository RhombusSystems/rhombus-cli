package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	liveCmd := &cobra.Command{
		Use:   "live [camera-name-or-uuid]",
		Short: "Open a live video stream in the browser",
		Long:  "Opens a live video feed from a Rhombus camera in your default browser. Accepts a camera UUID or partial name for fuzzy matching.",
		Args:  cobra.ExactArgs(1),
		RunE:  runLive,
	}
	liveCmd.Flags().Int("duration", 3600, "Federated token duration in seconds")
	rootCmd.AddCommand(liveCmd)
}

func runLive(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	duration, _ := cmd.Flags().GetInt("duration")
	cameraArg := args[0]

	// Resolve camera UUID
	cameraUUID, cameraName, err := resolveCamera(cfg, cameraArg)
	if err != nil {
		return err
	}

	fmt.Printf("Opening live stream for %s...\n", cameraName)

	// Generate federated session token
	fedResp, err := client.APICall(cfg, "/api/org/generateFederatedSessionToken", map[string]any{
		"durationSec": duration,
	})
	if err != nil {
		return fmt.Errorf("generating federated token: %w", err)
	}
	federatedToken, _ := fedResp["federatedSessionToken"].(string)
	if federatedToken == "" {
		return fmt.Errorf("no federated token returned")
	}

	// Get media URIs
	mediaResp, err := client.APICall(cfg, "/api/camera/getMediaUris", map[string]any{
		"cameraUuid": cameraUUID,
	})
	if err != nil {
		return fmt.Errorf("getting media URIs: %w", err)
	}

	mpdUri, _ := mediaResp["wanLiveMpdUri"].(string)
	if mpdUri == "" {
		return fmt.Errorf("no live MPD URI available for this camera")
	}

	// Append federated token auth
	streamURL := mpdUri + "?x-auth-scheme=federated-token&x-auth-ft=" + federatedToken

	// Generate HTML file
	htmlPath, err := generatePlayerHTML(cameraName, streamURL)
	if err != nil {
		return fmt.Errorf("generating player: %w", err)
	}

	// Open in browser
	openInBrowser("file://" + htmlPath)

	fmt.Printf("Live stream opened in browser.\n")
	fmt.Printf("Token expires in %d seconds.\n", duration)
	return nil
}

func resolveCamera(cfg config.Config, cameraArg string) (uuid string, name string, err error) {
	// If it looks like a UUID (22 chars base64), use directly
	if looksLikeUUID(cameraArg) {
		return cameraArg, cameraArg, nil
	}

	// Otherwise, fuzzy match by name
	resp, err := client.APICall(cfg, "/api/camera/getMinimalCameraStateList", map[string]any{})
	if err != nil {
		return "", "", fmt.Errorf("fetching cameras: %w", err)
	}

	cameras, _ := resp["cameraStates"].([]any)
	if len(cameras) == 0 {
		return "", "", fmt.Errorf("no cameras found")
	}

	search := cameraArg
	var matches []struct{ uuid, name string }

	for _, c := range cameras {
		cam, ok := c.(map[string]any)
		if !ok {
			continue
		}
		camName, _ := cam["name"].(string)
		camUUID, _ := cam["uuid"].(string)
		if camName == "" || camUUID == "" {
			continue
		}
		if containsFold(camName, search) {
			matches = append(matches, struct{ uuid, name string }{camUUID, camName})
		}
	}

	if len(matches) == 0 {
		return "", "", fmt.Errorf("no cameras matching \"%s\"", cameraArg)
	}
	if len(matches) == 1 {
		return matches[0].uuid, matches[0].name, nil
	}

	// Multiple matches — pick the first exact-ish match or list them
	fmt.Fprintf(os.Stderr, "Multiple cameras match \"%s\":\n", cameraArg)
	for i, m := range matches {
		fmt.Fprintf(os.Stderr, "  [%d] %s (%s)\n", i+1, m.name, m.uuid)
	}

	// For non-interactive use, pick the first
	return matches[0].uuid, matches[0].name, nil
}

func containsFold(s, substr string) bool {
	sLower := make([]byte, len(s))
	subLower := make([]byte, len(substr))
	for i := range s {
		if s[i] >= 'A' && s[i] <= 'Z' {
			sLower[i] = s[i] + 32
		} else {
			sLower[i] = s[i]
		}
	}
	for i := range substr {
		if substr[i] >= 'A' && substr[i] <= 'Z' {
			subLower[i] = substr[i] + 32
		} else {
			subLower[i] = substr[i]
		}
	}
	return contains(string(sLower), string(subLower))
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func generatePlayerHTML(cameraName, streamURL string) (string, error) {
	tmpDir := filepath.Join(os.TempDir(), "rhombus-live")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}

	htmlPath := filepath.Join(tmpDir, "live.html")

	// Escape for JSON embedding
	streamURLJSON, _ := json.Marshal(streamURL)
	cameraNameJSON, _ := json.Marshal(cameraName)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<title>%s — Rhombus Live</title>
<script src="https://cdn.dashjs.org/latest/dash.all.min.js"></script>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Nunito+Sans:wght@400;700;900&display=swap" rel="stylesheet">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: 'Nunito Sans', sans-serif;
    background: #0B0C0D;
    color: #FCFEFF;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    min-height: 100vh;
  }
  .container {
    width: 100%%;
    max-width: 1280px;
    padding: 20px;
  }
  .header {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 16px;
  }
  .live-badge {
    background: #D7331A;
    color: white;
    font-size: 11px;
    font-weight: 700;
    padding: 3px 8px;
    border-radius: 4px;
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }
  h1 {
    font-size: 20px;
    font-weight: 700;
    color: #FCFEFF;
  }
  video {
    width: 100%%;
    border-radius: 8px;
    background: #16171A;
  }
  .status {
    margin-top: 12px;
    font-size: 13px;
    color: #AEB3B8;
  }
  .status.error { color: #D7331A; }
  .status.connected { color: #6ABF02; }
</style>
</head>
<body>
<div class="container">
  <div class="header">
    <span class="live-badge">Live</span>
    <h1 id="camera-name"></h1>
  </div>
  <video id="player" autoplay muted></video>
  <div id="status" class="status">Connecting...</div>
</div>
<script>
  const streamUrl = %s;
  const cameraName = %s;

  document.getElementById('camera-name').textContent = cameraName;
  document.title = cameraName + ' — Rhombus Live';

  const video = document.getElementById('player');
  const status = document.getElementById('status');

  const authParams = 'x-auth-scheme=federated-token&x-auth-ft=' + streamUrl.split('x-auth-ft=')[1];

  const player = dashjs.MediaPlayer().create();

  player.updateSettings({
    streaming: {
      delay: { liveDelay: 2 },
      liveCatchup: { enabled: true, mode: 'liveCatchup', playbackRate: { min: -0.5, max: 0.5 } },
      buffer: { fastSwitchEnabled: true }
    }
  });

  // Intercept all XHR requests to append auth params to segment URLs
  const origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(method, url) {
    if (typeof url === 'string' && url.includes('dash.rhombussystems.com') && !url.includes('x-auth-ft=')) {
      url = url + (url.includes('?') ? '&' : '?') + authParams;
    }
    return origOpen.apply(this, [method, url, ...Array.prototype.slice.call(arguments, 2)]);
  };

  player.on(dashjs.MediaPlayer.events.PLAYBACK_STARTED, function() {
    status.textContent = 'Connected — Live';
    status.className = 'status connected';
  });

  player.on(dashjs.MediaPlayer.events.ERROR, function(e) {
    status.textContent = 'Stream error: ' + (e.error?.message || 'unknown');
    status.className = 'status error';
  });

  player.on(dashjs.MediaPlayer.events.PLAYBACK_ERROR, function(e) {
    status.textContent = 'Playback error — retrying...';
    status.className = 'status error';
    setTimeout(function() { player.attachSource(streamUrl); }, 3000);
  });

  player.initialize(video, streamUrl, true);
  video.addEventListener('click', function() {
    if (video.muted) { video.muted = false; }
  });
</script>
</body>
</html>`, cameraName, streamURLJSON, cameraNameJSON)

	if err := os.WriteFile(htmlPath, []byte(html), 0644); err != nil {
		return "", err
	}

	return htmlPath, nil
}

func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}
