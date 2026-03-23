package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	footageCmd := &cobra.Command{
		Use:   "footage [camera-name-or-uuid]",
		Short: "View camera footage in the browser",
		Long:  "Opens a Rhombus camera player in the browser. Defaults to live view. Use --start to jump to a specific time in the past.",
		Args:  cobra.ExactArgs(1),
		RunE:  runFootage,
	}
	footageCmd.Flags().String("start", "", "Start time (epoch ms, or relative like '5m ago', '1h ago'). Defaults to live.")
	footageCmd.Flags().Int("token-duration", 3600, "Federated token duration in seconds")
	rootCmd.AddCommand(footageCmd)
}

func runFootage(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	duration, _ := cmd.Flags().GetInt("token-duration")
	startStr, _ := cmd.Flags().GetString("start")
	cameraArg := args[0]

	// Resolve camera UUID
	cameraUUID, cameraName, err := resolveCamera(cfg, cameraArg)
	if err != nil {
		return err
	}

	if startStr != "" {
		startMs, err := parseTimestamp(startStr)
		if err != nil {
			return fmt.Errorf("invalid start time: %w", err)
		}
		fmt.Printf("Opening footage for %s at %s...\n", cameraName,
			time.UnixMilli(startMs).Format("Jan 2 3:04:05 PM"))
	} else {
		fmt.Printf("Opening live view for %s...\n", cameraName)
	}

	// Start the local server first so we know the port
	serverURL, _, err := startPlayerServer(cameraUUID, cameraName, cfg, duration)
	if err != nil {
		return fmt.Errorf("starting player server: %w", err)
	}

	// Append start time if specified
	if startStr != "" {
		startMs, _ := parseTimestamp(startStr)
		serverURL += fmt.Sprintf("&t=%d", startMs)
	}

	openInBrowserNewWindow(serverURL)

	fmt.Println("Player opened in browser.")
	fmt.Println("Press Ctrl+C to stop.")

	// Keep the process alive so the local HTTP server stays running
	select {}
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
	openInBrowserImpl(url, false)
}

func openInBrowserNewWindow(url string) {
	openInBrowserImpl(url, true)
}

func openInBrowserImpl(url string, newWindow bool) {
	switch runtime.GOOS {
	case "darwin":
		if newWindow {
			// Try Chrome first, then fall back to default
			cmd := exec.Command("open", "-na", "Google Chrome", "--args", "--new-window", url)
			if cmd.Start() == nil {
				return
			}
			// Fallback to default browser
			exec.Command("open", url).Start()
			return
		}
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	case "windows":
		if newWindow {
			exec.Command("cmd", "/c", "start", "chrome", "--new-window", url).Start()
			return
		}
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}

const (
	apiPlayerAssetsURL = "https://public-bucket-itg.s3.us-west-2.amazonaws.com/rhombus-cli"
	apiPlayerJSFile    = "index-_4DoYqly.js"
	apiPlayerCSSFile   = "index-CE1zZXB9.css"
)

// ensureApiPlayerAssets downloads the player JS/CSS to ~/.rhombus/player/ if not already cached.
func ensureApiPlayerAssets() (string, error) {
	playerDir := filepath.Join(rhombusDir(), "player", "assets")
	if err := os.MkdirAll(playerDir, 0755); err != nil {
		return "", fmt.Errorf("creating player dir: %w", err)
	}

	for _, file := range []string{apiPlayerJSFile, apiPlayerCSSFile} {
		localPath := filepath.Join(playerDir, file)
		if _, err := os.Stat(localPath); err == nil {
			continue // already cached
		}

		url := apiPlayerAssetsURL + "/assets/" + file
		fmt.Printf("Downloading player asset: %s\n", file)

		resp, err := http.Get(url)
		if err != nil {
			return "", fmt.Errorf("downloading %s: %w", file, err)
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			return "", fmt.Errorf("downloading %s: HTTP %d", file, resp.StatusCode)
		}

		f, err := os.Create(localPath)
		if err != nil {
			resp.Body.Close()
			return "", fmt.Errorf("creating %s: %w", file, err)
		}
		io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
	}

	return playerDir, nil
}

func startPlayerServer(cameraUUID, cameraName string, cfg config.Config, duration int) (string, int, error) {
	assetsDir, err := ensureApiPlayerAssets()
	if err != nil {
		return "", 0, err
	}

	playerDir := filepath.Dir(assetsDir)

	// Start local HTTP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, fmt.Errorf("starting local server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	origin := fmt.Sprintf("http://localhost:%d", port)

	// Generate initial federated token with the correct domain
	federatedToken, err := generateFederatedToken(cfg, duration, origin)
	if err != nil {
		return "", 0, err
	}

	// Write the HTML
	htmlPath := filepath.Join(playerDir, "player.html")
	if err := writePlayerHTML(htmlPath, cameraName, cameraUUID, federatedToken); err != nil {
		return "", 0, err
	}

	// Auto-refresh the token every 50 minutes (before the 60min expiry)
	go func() {
		ticker := time.NewTicker(50 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			newToken, err := generateFederatedToken(cfg, duration, origin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nWarning: failed to refresh token: %v\n", err)
				continue
			}
			federatedToken = newToken
			writePlayerHTML(htmlPath, cameraName, cameraUUID, federatedToken)
			fmt.Fprintf(os.Stderr, "\nToken refreshed.\n")
		}
	}()

	// Serve local assets, proxy missing from remote, SPA fallback
	remoteBase := apiPlayerAssetsURL
	go func() {
		mux := http.NewServeMux()
		fs := http.FileServer(http.Dir(playerDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			localPath := filepath.Join(playerDir, r.URL.Path)
			if info, statErr := os.Stat(localPath); statErr == nil && !info.IsDir() {
				fs.ServeHTTP(w, r)
				return
			}

			if strings.HasPrefix(r.URL.Path, "/assets/") ||
				strings.HasSuffix(r.URL.Path, ".js") ||
				strings.HasSuffix(r.URL.Path, ".css") ||
				strings.HasSuffix(r.URL.Path, ".wasm") ||
				strings.HasSuffix(r.URL.Path, ".png") ||
				strings.HasSuffix(r.URL.Path, ".svg") ||
				strings.HasSuffix(r.URL.Path, ".woff") ||
				strings.HasSuffix(r.URL.Path, ".woff2") {
				remoteURL := remoteBase + r.URL.Path
				proxyResp, proxyErr := http.Get(remoteURL)
				if proxyErr == nil && proxyResp.StatusCode == 200 {
					for k, v := range proxyResp.Header {
						w.Header()[k] = v
					}
					io.Copy(w, proxyResp.Body)
					proxyResp.Body.Close()
					return
				}
				if proxyResp != nil {
					proxyResp.Body.Close()
				}
			}

			http.ServeFile(w, r, htmlPath)
		})
		http.Serve(listener, mux)
	}()

	playerURL := fmt.Sprintf("%s/api/player/%s?ft=%s&name=%s",
		origin, cameraUUID, federatedToken, cameraName)
	return playerURL, port, nil
}

func generateFederatedToken(cfg config.Config, duration int, domain string) (string, error) {
	fedResp, err := client.APICall(cfg, "/api/org/generateFederatedSessionToken", map[string]any{
		"durationSec": duration,
		"domain":      domain,
	})
	if err != nil {
		return "", fmt.Errorf("generating federated token: %w", err)
	}
	token, _ := fedResp["federatedSessionToken"].(string)
	if token == "" {
		return "", fmt.Errorf("no federated token returned")
	}
	return token, nil
}

func writePlayerHTML(htmlPath, cameraName, cameraUUID, federatedToken string) error {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>%s — Rhombus Player</title>
  <link rel="stylesheet" href="/assets/%s" />
  <style>
    html, body, #root { height: 100%%; margin: 0; }
  </style>
</head>
<body>
  <div id="root" style="height: 100%%"></div>
  <script type="module" src="/assets/%s"></script>
</body>
</html>`, cameraName, apiPlayerCSSFile, apiPlayerJSFile)

	return os.WriteFile(htmlPath, []byte(html), 0644)
}
