package cmd

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

type eventClip struct {
	Camera     string
	CameraUUID string
	StartMs    int64
	EndMs      int64
}

type eventGroup struct {
	Clips   []eventClip
	StartMs int64
	EndMs   int64
}

type vodTemplate struct {
	lan string // LAN template (preferred, faster)
	wan string // WAN template (fallback)
}

func init() {
	stitchCmd := &cobra.Command{
		Use:   "stitch",
		Short: "Create a stitched video of events across cameras",
		Long: `Download video clips for detected events and stitch them into a single
chronological video. Concurrent events from multiple cameras are shown
in a grid layout. Timestamps are overlaid on each clip.`,
		RunE: runStitch,
	}
	stitchCmd.Flags().String("location", "", "Location name (all cameras)")
	stitchCmd.Flags().StringSlice("camera", nil, "Camera names or UUIDs (comma-separated)")
	stitchCmd.Flags().String("start", "", "Start time (epoch ms)")
	stitchCmd.Flags().String("end", "", "End time (epoch ms)")
	stitchCmd.Flags().String("period", "", "Natural language time window (e.g., 'yesterday between 6am and 7am')")
	stitchCmd.Flags().Int("buffer", 5, "Seconds of buffer around each event")
	stitchCmd.Flags().Bool("include-motion", false, "Include motion seekpoints (default: only human/vehicle/object activity)")
	stitchCmd.Flags().String("output", "", "Output file path (default: auto-generated)")
	rootCmd.AddCommand(stitchCmd)
}

func runStitch(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	locationName, _ := cmd.Flags().GetString("location")
	cameraNames, _ := cmd.Flags().GetStringSlice("camera")
	startStr, _ := cmd.Flags().GetString("start")
	endStr, _ := cmd.Flags().GetString("end")
	periodStr, _ := cmd.Flags().GetString("period")
	buffer, _ := cmd.Flags().GetInt("buffer")
	includeMotion, _ := cmd.Flags().GetBool("include-motion")
	outputPath, _ := cmd.Flags().GetString("output")

	// Check ffmpeg
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required. Install with: brew install ffmpeg")
	}

	// Resolve time range
	var startMs, endMs int64
	if periodStr != "" {
		var err error
		startMs, endMs, err = parsePeriod(periodStr)
		if err != nil {
			return fmt.Errorf("parsing period: %w", err)
		}
	} else if startStr != "" && endStr != "" {
		var err error
		startMs, err = parseTimestamp(startStr)
		if err != nil {
			return fmt.Errorf("invalid start: %w", err)
		}
		endMs, err = parseTimestamp(endStr)
		if err != nil {
			return fmt.Errorf("invalid end: %w", err)
		}
	} else {
		return fmt.Errorf("specify --start and --end, or --period")
	}

	fmt.Printf("Time window: %s to %s\n",
		time.UnixMilli(startMs).Format("Jan 2 3:04 PM"),
		time.UnixMilli(endMs).Format("Jan 2 3:04 PM"))

	// Resolve cameras
	var cameraUUIDs []string
	cameraNameMap := getCameraNameMap(cfg)

	if locationName != "" {
		var err error
		cameraUUIDs, cameraNameMap, err = resolveCamerasAtLocation(cfg, locationName)
		if err != nil {
			return err
		}
	} else if len(cameraNames) > 0 {
		for _, name := range cameraNames {
			uuid, _, err := resolveCamera(cfg, name)
			if err != nil {
				return err
			}
			cameraUUIDs = append(cameraUUIDs, uuid)
		}
	} else {
		return fmt.Errorf("specify --camera or --location")
	}

	fmt.Printf("Cameras: %d\n", len(cameraUUIDs))

	// Get VOD URI templates for each camera upfront (LAN preferred, WAN fallback)
	fmt.Println("Fetching media URIs...")
	vodTemplates := make(map[string]vodTemplate)
	for _, camUUID := range cameraUUIDs {
		resp, err := client.APICall(cfg, "/api/camera/getMediaUris", map[string]any{
			"cameraUuid": camUUID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: couldn't get media URIs for %s: %v\n", camUUID, err)
			continue
		}
		vt := vodTemplate{}
		// LAN templates come as an array
		if lanTemplates, ok := resp["lanVodMpdUrisTemplates"].([]any); ok && len(lanTemplates) > 0 {
			if t, ok := lanTemplates[0].(string); ok {
				vt.lan = t
			}
		}
		if t, _ := resp["wanVodMpdUriTemplate"].(string); t != "" {
			// Use dash-internal for cert-based WAN auth
			vt.wan = strings.Replace(t, ".dash.rhombussystems.com", ".dash-internal.rhombussystems.com", 1)
		}
		if vt.lan != "" || vt.wan != "" {
			vodTemplates[camUUID] = vt
		}
	}

	// Generate federated token for LAN access
	var fedToken string
	fedResp, err := client.APICall(cfg, "/api/org/generateFederatedSessionToken", map[string]any{
		"durationSec": 3600,
	})
	if err == nil {
		fedToken, _ = fedResp["federatedSessionToken"].(string)
	}
	if fedToken != "" {
		fmt.Println("Using LAN streaming (federated token)")
	} else {
		fmt.Println("Using WAN streaming (cert auth)")
	}

	// Get seekpoints for each camera to find events
	fmt.Println("Finding events...")
	var allEvents []eventClip
	bufferMs := int64(buffer) * 1000

	for _, camUUID := range cameraUUIDs {
		vt, ok := vodTemplates[camUUID]
		if !ok || (vt.lan == "" && vt.wan == "") {
			continue // skip cameras without VOD template
		}

		camName := cameraNameMap[camUUID]
		if camName == "" {
			camName = camUUID
		}

		activityTimes := getActivityTimes(cfg, camUUID, startMs, endMs, includeMotion)
		if len(activityTimes) == 0 {
			continue
		}

		// Cluster activity into events (gap > 10s = new event)
		clusterGapMs := int64(10000)
		// Minimum number of seekpoints in a cluster to be considered a real event
		// (filters out single-frame blips that produce mostly dead time)
		minSeekpointsPerEvent := 2

		clusterStart := activityTimes[0]
		clusterEnd := activityTimes[0]
		clusterCount := 1

		for i := 1; i < len(activityTimes); i++ {
			if activityTimes[i]-clusterEnd > clusterGapMs {
				if clusterCount >= minSeekpointsPerEvent {
					allEvents = append(allEvents, eventClip{
						Camera:     camName,
						CameraUUID: camUUID,
						StartMs:    clusterStart - bufferMs,
						EndMs:      clusterEnd + bufferMs,
					})
				}
				clusterStart = activityTimes[i]
				clusterCount = 0
			}
			clusterEnd = activityTimes[i]
			clusterCount++
		}
		if clusterCount >= minSeekpointsPerEvent {
			allEvents = append(allEvents, eventClip{
				Camera:     camName,
				CameraUUID: camUUID,
				StartMs:    clusterStart - bufferMs,
				EndMs:      clusterEnd + bufferMs,
			})
		}
	}

	if len(allEvents) == 0 {
		fmt.Println("No events found in this time window.")
		return nil
	}

	// Clamp to window
	for i := range allEvents {
		if allEvents[i].StartMs < startMs {
			allEvents[i].StartMs = startMs
		}
		if allEvents[i].EndMs > endMs {
			allEvents[i].EndMs = endMs
		}
	}

	sort.Slice(allEvents, func(i, j int) bool { return allEvents[i].StartMs < allEvents[j].StartMs })

	// Group overlapping events
	groups := groupOverlappingEvents(allEvents)
	fmt.Printf("Events: %d across %d time segments\n", len(allEvents), len(groups))

	// Working directory
	workDir := filepath.Join(os.TempDir(), "rhombus-stitch", fmt.Sprintf("%d", time.Now().Unix()))
	os.MkdirAll(workDir, 0755)

	// Process each group
	var segmentPaths []string
	for gi, group := range groups {
		groupStart := time.UnixMilli(group.StartMs)
		groupEnd := time.UnixMilli(group.EndMs)
		durSec := float64(group.EndMs-group.StartMs) / 1000
		cams := make([]string, len(group.Clips))
		for i, c := range group.Clips {
			cams[i] = c.Camera
		}
		fmt.Printf("\nSegment %d/%d: %s - %s (%.0fs) [%s]\n",
			gi+1, len(groups),
			groupStart.Format("3:04:05 PM"),
			groupEnd.Format("3:04:05 PM"),
			durSec, strings.Join(cams, ", "))

		segPath, err := processGroupVOD(cfg, group, gi, workDir, vodTemplates, fedToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			continue
		}
		segmentPaths = append(segmentPaths, segPath)
	}

	if len(segmentPaths) == 0 {
		return fmt.Errorf("no segments created")
	}

	// Concatenate all segments
	if outputPath == "" {
		outputPath = fmt.Sprintf("stitch_%s.mp4",
			time.UnixMilli(startMs).Format("2006-01-02_15-04"))
	}

	fmt.Printf("\nStitching %d segments...\n", len(segmentPaths))
	if err := concatenateSegments(segmentPaths, outputPath); err != nil {
		return fmt.Errorf("concatenation failed: %w", err)
	}

	info, _ := os.Stat(outputPath)
	fmt.Printf("Video saved: %s (%.1f MB)\n", outputPath, float64(info.Size())/1024/1024)

	absPath, _ := filepath.Abs(outputPath)
	openInBrowserNewWindow("file://" + absPath)
	return nil
}

func groupOverlappingEvents(events []eventClip) []eventGroup {
	if len(events) == 0 {
		return nil
	}

	var groups []eventGroup
	current := eventGroup{
		Clips:   []eventClip{events[0]},
		StartMs: events[0].StartMs,
		EndMs:   events[0].EndMs,
	}

	for i := 1; i < len(events); i++ {
		if events[i].StartMs <= current.EndMs {
			current.Clips = append(current.Clips, events[i])
			if events[i].EndMs > current.EndMs {
				current.EndMs = events[i].EndMs
			}
		} else {
			groups = append(groups, current)
			current = eventGroup{
				Clips:   []eventClip{events[i]},
				StartMs: events[i].StartMs,
				EndMs:   events[i].EndMs,
			}
		}
	}
	groups = append(groups, current)

	// Merge duplicate cameras within each group
	for i := range groups {
		merged := make(map[string]eventClip)
		for _, c := range groups[i].Clips {
			if existing, ok := merged[c.CameraUUID]; ok {
				if c.StartMs < existing.StartMs {
					existing.StartMs = c.StartMs
				}
				if c.EndMs > existing.EndMs {
					existing.EndMs = c.EndMs
				}
				merged[c.CameraUUID] = existing
			} else {
				merged[c.CameraUUID] = c
			}
		}
		groups[i].Clips = groups[i].Clips[:0]
		for _, c := range merged {
			groups[i].Clips = append(groups[i].Clips, c)
		}
		sort.Slice(groups[i].Clips, func(a, b int) bool {
			return groups[i].Clips[a].Camera < groups[i].Clips[b].Camera
		})
	}

	return groups
}

// downloadVODClipLAN downloads a VOD clip via LAN using federated token auth.
// LAN templates use {START_TIME}_{DURATION}/clip.mpd format.
func downloadVODClipLAN(cfg config.Config, lanTemplate string, startSec, durationSec int64, outputPath, fedToken string) error {
	mpdURL := strings.Replace(lanTemplate, "{START_TIME}", fmt.Sprintf("%d", startSec), 1)
	mpdURL = strings.Replace(mpdURL, "{DURATION}", fmt.Sprintf("%d", durationSec), 1)

	setHeaders := func(req *http.Request) {
		req.Header.Set("Cookie", "RHOMBUS_SESSIONID=RFT:"+fedToken)
		req.Header.Set("x-auth-scheme", "api-token")
		req.Header.Set("x-auth-apikey", cfg.ApiKey)
	}
	httpClient, _ := client.GetMediaHTTPClient(cfg)
	return downloadVODClipFromMPD(httpClient, mpdURL, durationSec, outputPath, setHeaders)
}

// downloadVODClipWAN downloads a VOD clip via WAN using cert-based auth on dash-internal.
// WAN templates use {START_TIME}/{DURATION}/vod/file.mpd format.
func downloadVODClipWAN(cfg config.Config, wanTemplate string, startSec, durationSec int64, outputPath string) error {
	mpdURL := strings.Replace(wanTemplate, "{START_TIME}", fmt.Sprintf("%d", startSec), 1)
	mpdURL = strings.Replace(mpdURL, "{DURATION}", fmt.Sprintf("%d", durationSec), 1)

	setHeaders := func(req *http.Request) {
		req.Header.Set("x-auth-scheme", "api")
		req.Header.Set("x-auth-apikey", cfg.ApiKey)
	}
	httpClient, _ := client.GetMediaHTTPClient(cfg)
	return downloadVODClipFromMPD(httpClient, mpdURL, durationSec, outputPath, setHeaders)
}

// downloadVODClipFromMPD fetches the MPD manifest, then downloads init + media segments.
func downloadVODClipFromMPD(httpClient *http.Client, mpdURL string, durationSec int64, outputPath string, setHeaders func(*http.Request)) error {
	mpdReq, err := http.NewRequest("GET", mpdURL, nil)
	if err != nil {
		return fmt.Errorf("creating MPD request: %w", err)
	}
	setHeaders(mpdReq)

	mpdResp, err := httpClient.Do(mpdReq)
	if err != nil {
		return fmt.Errorf("fetching MPD: %w", err)
	}
	defer mpdResp.Body.Close()

	if mpdResp.StatusCode != 200 {
		body, _ := io.ReadAll(mpdResp.Body)
		return fmt.Errorf("MPD request failed (HTTP %d): %s", mpdResp.StatusCode, string(body))
	}

	// Consume the MPD body (we only need segment count, which is duration/2)
	io.ReadAll(mpdResp.Body)

	numSegments := durationSec / 2
	if numSegments < 1 {
		numSegments = 1
	}

	baseURL := mpdURL[:strings.LastIndex(mpdURL, "/")+1]
	tmpDir := outputPath + ".segments"
	os.MkdirAll(tmpDir, 0755)
	defer os.RemoveAll(tmpDir)

	// Download init segment
	initPath := filepath.Join(tmpDir, "seg_init.mp4")
	if err := downloadDashSegment(httpClient, baseURL+"seg_init.mp4", initPath, setHeaders); err != nil {
		return fmt.Errorf("downloading init segment: %w", err)
	}

	// Concatenate init + media segments into output
	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	initData, err := os.ReadFile(initPath)
	if err != nil {
		return err
	}
	outFile.Write(initData)

	for i := int64(1); i <= numSegments; i++ {
		segPath := filepath.Join(tmpDir, fmt.Sprintf("seg_%d.m4v", i))
		segURL := fmt.Sprintf("%sseg_%d.m4v", baseURL, i)
		if err := downloadDashSegment(httpClient, segURL, segPath, setHeaders); err != nil {
			break
		}
		segData, err := os.ReadFile(segPath)
		if err != nil {
			break
		}
		outFile.Write(segData)
	}

	return nil
}

func downloadDashSegment(httpClient *http.Client, url, outputPath string, setHeaders func(*http.Request)) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	setHeaders(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// processGroupVOD downloads actual video clips for each camera in the group,
// adds overlays, and tiles them if multiple cameras.
func processGroupVOD(cfg config.Config, group eventGroup, groupIndex int, workDir string, vodTemplates map[string]vodTemplate, fedToken string) (string, error) {
	groupDir := filepath.Join(workDir, fmt.Sprintf("group_%02d", groupIndex))
	os.MkdirAll(groupDir, 0755)

	// Use the group's time range for all clips (so multi-cam clips align)
	startSec := group.StartMs / 1000
	durationSec := (group.EndMs - group.StartMs) / 1000
	if durationSec < 2 {
		durationSec = 2
	}

	var clips []camClip

	for _, clip := range group.Clips {
		vt, ok := vodTemplates[clip.CameraUUID]
		if !ok {
			continue
		}

		rawPath := filepath.Join(groupDir, sanitizeName(clip.Camera)+"_raw.mp4")
		var dlErr error
		// Try LAN first (faster, direct to device), fall back to WAN
		if vt.lan != "" && fedToken != "" {
			dlErr = downloadVODClipLAN(cfg, vt.lan, startSec, durationSec, rawPath, fedToken)
		}
		if dlErr != nil || vt.lan == "" || fedToken == "" {
			if vt.wan != "" {
				dlErr = downloadVODClipWAN(cfg, vt.wan, startSec, durationSec, rawPath)
			} else {
				dlErr = fmt.Errorf("no WAN template available")
			}
		}
		if dlErr != nil {
			fmt.Fprintf(os.Stderr, "  %s: download failed: %v\n", clip.Camera, dlErr)
			continue
		}

		// Add camera name + date/time overlay
		overlayPath := filepath.Join(groupDir, sanitizeName(clip.Camera)+".mp4")
		startEpoch := group.StartMs / 1000
		// drawtext with camera name on line 1, running clock on line 2
		// pts:localtime adds PTS (seconds from start) to the epoch to show wall-clock time
		drawtext := fmt.Sprintf(
			`drawtext=text='%s':fontcolor=white:fontsize=24:box=1:boxcolor=black@0.6:boxborderw=6:x=16:y=16,`+
				`drawtext=text='%%{pts\:localtime\:%d}':fontcolor=white:fontsize=20:box=1:boxcolor=black@0.6:boxborderw=4:x=16:y=52`,
			clip.Camera, startEpoch)

		vf := fmt.Sprintf("scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black,fps=15,%s", drawtext)
		overlayCmd := exec.Command("ffmpeg",
			"-i", rawPath,
			"-vf", vf,
			"-c:v", "libx264", "-preset", "fast", "-crf", "23",
			"-pix_fmt", "yuv420p",
			"-an", // drop audio
			overlayPath, "-y")
		overlayCmd.Stderr = nil
		if err := overlayCmd.Run(); err != nil {
			// Fall back to raw clip without overlay
			overlayPath = rawPath
		}

		info, _ := os.Stat(overlayPath)
		fmt.Printf("  %s: %.1fs (%.1f KB)\n", clip.Camera, float64(durationSec), float64(info.Size())/1024)
		clips = append(clips, camClip{camera: clip.Camera, path: overlayPath})
	}

	if len(clips) == 0 {
		return "", fmt.Errorf("no clips downloaded")
	}

	segPath := filepath.Join(workDir, fmt.Sprintf("segment_%02d.mp4", groupIndex))

	if len(clips) == 1 {
		// Single camera — already 1280x720@15fps from overlay step
		cpCmd := exec.Command("ffmpeg",
			"-i", clips[0].path,
			"-c", "copy",
			segPath, "-y")
		cpCmd.Stderr = nil
		if err := cpCmd.Run(); err != nil {
			return "", fmt.Errorf("remux failed: %w", err)
		}
		return segPath, nil
	}

	// Multiple cameras — tile them, then normalize to 1280x720@15fps
	tiledRaw := filepath.Join(groupDir, "tiled_raw.mp4")
	if _, err := createTiledClips(clips, tiledRaw); err != nil {
		return "", err
	}
	// Normalize tiled output to consistent resolution and framerate
	normCmd := exec.Command("ffmpeg",
		"-i", tiledRaw,
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black,fps=15",
		"-c:v", "libx264", "-preset", "fast", "-crf", "23",
		"-pix_fmt", "yuv420p",
		"-video_track_timescale", "15360",
		segPath, "-y")
	normCmd.Stderr = nil
	if err := normCmd.Run(); err != nil {
		return "", fmt.Errorf("normalize tiled: %w", err)
	}
	return segPath, nil
}

func createTiledClips(clips []camClip, outputPath string) (string, error) {
	numCams := len(clips)
	cols := int(math.Ceil(math.Sqrt(float64(numCams))))
	rows := int(math.Ceil(float64(numCams) / float64(cols)))

	tileW := 640
	tileH := 360

	// Build ffmpeg command with xstack filter
	var inputs []string
	var scaleFilters []string
	var stackLabels []string

	for i, c := range clips {
		inputs = append(inputs, "-i", c.path)
		scaleFilters = append(scaleFilters,
			fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1[v%d]",
				i, tileW, tileH, tileW, tileH, i))
		stackLabels = append(stackLabels, fmt.Sprintf("[v%d]", i))
	}

	// Pad to fill grid if needed (duplicate last stream)
	totalCells := cols * rows
	for i := numCams; i < totalCells; i++ {
		// Use a null source for empty cells
		scaleFilters = append(scaleFilters,
			fmt.Sprintf("color=black:s=%dx%d:d=1[v%d]", tileW, tileH, i))
		stackLabels = append(stackLabels, fmt.Sprintf("[v%d]", i))
	}

	// Build layout string for xstack
	var layoutParts []string
	for i := 0; i < totalCells; i++ {
		x := (i % cols) * tileW
		y := (i / cols) * tileH
		layoutParts = append(layoutParts, fmt.Sprintf("%d_%d", x, y))
	}

	filter := strings.Join(scaleFilters, ";")
	filter += ";" + strings.Join(stackLabels, "") + fmt.Sprintf("xstack=inputs=%d:layout=%s[out]",
		totalCells, strings.Join(layoutParts, "|"))

	args := append(inputs,
		"-filter_complex", filter,
		"-map", "[out]",
		"-c:v", "libx264", "-preset", "fast", "-crf", "23",
		"-shortest",
		outputPath, "-y")

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg tile: %w\n%s", err, string(output))
	}
	return outputPath, nil
}

func concatenateSegments(segmentPaths []string, outputPath string) error {
	listPath := outputPath + ".concat.txt"
	f, err := os.Create(listPath)
	if err != nil {
		return err
	}
	for _, sp := range segmentPaths {
		fmt.Fprintf(f, "file '%s'\n", sp)
	}
	f.Close()
	defer os.Remove(listPath)

	cmd := exec.Command("ffmpeg",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		outputPath, "-y")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(output))
	}
	return nil
}

type camClip struct {
	camera string
	path   string
}
