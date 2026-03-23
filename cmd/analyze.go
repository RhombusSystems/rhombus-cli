package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

const (
	targetFramesPerCamera = 100
	minFrameIntervalSec   = 2
	maxFrameIntervalSec   = 60
)

type AnalysisManifest struct {
	Camera    string          `json:"camera"`
	CameraID string          `json:"cameraUuid"`
	TimeRange TimeRangeJSON   `json:"timeRange"`
	Frames    []FrameEntry    `json:"frames"`
}

type TimeRangeJSON struct {
	StartMs int64 `json:"startMs"`
	EndMs   int64 `json:"endMs"`
}

type FrameEntry struct {
	TimestampMs int64  `json:"timestampMs"`
	Path        string `json:"path"`
	HasActivity bool   `json:"hasActivity"`
}

func init() {
	analyzeCmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze camera footage and alerts",
		Long:  "Extract and analyze frames from alert clips or camera footage over a time window.",
	}

	alertCmd := &cobra.Command{
		Use:   "alert <alert-uuid>",
		Short: "Analyze an alert's video frames",
		Args:  cobra.ExactArgs(1),
		RunE:  runAnalyzeAlert,
	}
	alertCmd.Flags().Bool("raw", false, "Output frames + manifest for external analysis (skip visual analysis)")
	alertCmd.Flags().String("output", "", "Output directory for frames (default: temp dir)")

	footageCmd := &cobra.Command{
		Use:   "footage [camera-names-or-uuids...]",
		Short: "Analyze camera footage over a time window",
		Args:  cobra.MinimumNArgs(0),
		RunE:  runAnalyzeFootage,
	}
	footageCmd.Flags().String("location", "", "Location name (resolves to all cameras at that location)")
	footageCmd.Flags().String("start", "", "Start time (epoch ms)")
	footageCmd.Flags().String("end", "", "End time (epoch ms)")
	footageCmd.Flags().String("period", "", "Natural language time window (e.g., 'yesterday between 8am and 9am')")
	footageCmd.Flags().Bool("raw", false, "Output frames + manifest for external analysis")
	footageCmd.Flags().Bool("fill", false, "Include evenly-spaced fill frames in addition to activity frames")
	footageCmd.Flags().Bool("include-motion", false, "Include motion seekpoints (default: only human/vehicle/object activity)")
	footageCmd.Flags().Bool("lan", false, "Download frames via LAN (faster, requires local network access to cameras)")
	footageCmd.Flags().String("output", "", "Output directory for frames (default: temp dir)")

	analyzeCmd.AddCommand(alertCmd)
	analyzeCmd.AddCommand(footageCmd)
	rootCmd.AddCommand(analyzeCmd)
}

// ─── Analyze Alert ──────────────────────────────────────────────────

func runAnalyzeAlert(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	alertUuid := args[0]
	raw, _ := cmd.Flags().GetBool("raw")
	outputDir, _ := cmd.Flags().GetString("output")

	if outputDir == "" {
		outputDir = filepath.Join(os.TempDir(), "rhombus-analyze", alertUuid)
	}
	os.MkdirAll(outputDir, 0755)

	// Get alert details
	alert, err := getAlertDetails(cfg, alertUuid)
	if err != nil {
		return err
	}

	deviceUuid, _ := alert["deviceUuid"].(string)
	tsMs, _ := alert["timestampMs"].(float64)
	durSec, _ := alert["durationSec"].(float64)

	cameraNames := getCameraNameMap(cfg)
	camName := cameraNames[deviceUuid]
	if camName == "" {
		camName = deviceUuid
	}

	fmt.Printf("Analyzing alert: %s at %s (%.0fs)\n", camName,
		time.UnixMilli(int64(tsMs)).Format("Jan 2 3:04:05 PM"), durSec)

	// Get bounding box timestamps
	boundingBoxes, _ := alert["boundingBoxes"].([]any)
	var bbTimes []float64
	for _, bb := range boundingBoxes {
		b, ok := bb.(map[string]any)
		if !ok {
			continue
		}
		relSec, _ := b["relativeSecond"].(float64)
		bbTimes = append(bbTimes, relSec)
	}

	// Deduplicate and sort
	bbTimes = dedup(bbTimes, 0.5)
	sort.Float64s(bbTimes)

	if len(bbTimes) == 0 {
		// No bounding boxes — sample evenly across duration
		interval := durSec / 5
		if interval < 1 {
			interval = 1
		}
		for t := 1.0; t < durSec; t += interval {
			bbTimes = append(bbTimes, t)
		}
	}

	// Download alert clip
	clipPath := filepath.Join(outputDir, "clip.mp4")
	fmt.Println("Downloading alert clip...")

	region := getAlertRegion(alert, "clipLocation")
	clipBaseURL := fmt.Sprintf("%s/media/metadata/%s/%s/%s",
		mediaBaseURL, deviceUuid, region, alertUuid)

	if err := downloadAlertClipToFile(cfg, clipBaseURL, clipPath); err != nil {
		return fmt.Errorf("downloading clip: %w", err)
	}

	// Extract frames at bounding box timestamps
	fmt.Printf("Extracting %d frames...\n", len(bbTimes))
	var frames []FrameEntry
	for i, relSec := range bbTimes {
		framePath := filepath.Join(outputDir, fmt.Sprintf("frame_%03d.jpeg", i+1))
		frameCmd := exec.Command("ffmpeg", "-i", clipPath, "-ss", fmt.Sprintf("%.2f", relSec),
			"-frames:v", "1", "-q:v", "2", framePath, "-y")
		frameCmd.Stderr = nil
		frameCmd.Stdout = nil
		if err := frameCmd.Run(); err != nil {
			continue
		}
		frames = append(frames, FrameEntry{
			TimestampMs: int64(tsMs) + int64(relSec*1000),
			Path:        framePath,
			HasActivity: true,
		})
	}

	manifest := AnalysisManifest{
		Camera:    camName,
		CameraID:  deviceUuid,
		TimeRange: TimeRangeJSON{StartMs: int64(tsMs), EndMs: int64(tsMs + durSec*1000)},
		Frames:    frames,
	}

	// Write manifest
	manifestPath := filepath.Join(outputDir, "manifest.json")
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(manifestPath, manifestJSON, 0644)

	if raw {
		fmt.Printf("Frames extracted to: %s\n", outputDir)
		fmt.Printf("Manifest: %s\n", manifestPath)
		fmt.Println(string(manifestJSON))
		return nil
	}

	// Visual analysis — read each frame and describe
	return visualAnalysis(manifest)
}

// ─── Analyze Footage ────────────────────────────────────────────────

func runAnalyzeFootage(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	raw, _ := cmd.Flags().GetBool("raw")
	fill, _ := cmd.Flags().GetBool("fill")
	includeMotion, _ := cmd.Flags().GetBool("include-motion")
	useLAN, _ := cmd.Flags().GetBool("lan")
	outputDir, _ := cmd.Flags().GetString("output")
	locationName, _ := cmd.Flags().GetString("location")
	startStr, _ := cmd.Flags().GetString("start")
	endStr, _ := cmd.Flags().GetString("end")
	periodStr, _ := cmd.Flags().GetString("period")

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

	windowSec := float64(endMs-startMs) / 1000
	fmt.Printf("Time window: %s to %s (%.0fs)\n",
		time.UnixMilli(startMs).Format("Jan 2 3:04 PM"),
		time.UnixMilli(endMs).Format("Jan 2 3:04 PM"),
		windowSec)

	// Resolve cameras
	var cameraUUIDs []string
	var cameraNameMap map[string]string

	if locationName != "" {
		var err error
		cameraUUIDs, cameraNameMap, err = resolveCamerasAtLocation(cfg, locationName)
		if err != nil {
			return err
		}
	} else if len(args) > 0 {
		cameraNameMap = getCameraNameMap(cfg)
		for _, arg := range args {
			uuid, _, err := resolveCamera(cfg, arg)
			if err != nil {
				return err
			}
			cameraUUIDs = append(cameraUUIDs, uuid)
		}
	} else {
		return fmt.Errorf("specify camera names/UUIDs as arguments, or use --location")
	}

	if len(cameraUUIDs) == 0 {
		return fmt.Errorf("no cameras found")
	}

	fmt.Printf("Cameras: %d\n", len(cameraUUIDs))

	if outputDir == "" {
		outputDir = filepath.Join(os.TempDir(), "rhombus-analyze", fmt.Sprintf("footage-%d", time.Now().Unix()))
	}
	os.MkdirAll(outputDir, 0755)

	// Calculate frame interval
	interval := windowSec / float64(targetFramesPerCamera)
	interval = math.Max(float64(minFrameIntervalSec), math.Min(float64(maxFrameIntervalSec), interval))
	numFrames := int(windowSec / interval)
	fmt.Printf("Target: %d frames per camera (every %.0fs)\n", numFrames, interval)

	// LAN mode: get media URIs and federated token
	var lanTemplates map[string]string
	var lanFedToken string
	if useLAN {
		lanTemplates = make(map[string]string)
		for _, camUUID := range cameraUUIDs {
			resp, err := client.APICall(cfg, "/api/camera/getMediaUris", map[string]any{
				"cameraUuid": camUUID,
			})
			if err != nil {
				continue
			}
			if templates, ok := resp["lanVodMpdUrisTemplates"].([]any); ok && len(templates) > 0 {
				if t, ok := templates[0].(string); ok {
					lanTemplates[camUUID] = t
				}
			}
		}
		fedResp, err := client.APICall(cfg, "/api/org/generateFederatedSessionToken", map[string]any{
			"durationSec": 3600,
		})
		if err == nil {
			lanFedToken, _ = fedResp["federatedSessionToken"].(string)
		}
		if lanFedToken != "" {
			fmt.Printf("LAN mode: %d cameras with LAN access\n", len(lanTemplates))
		} else {
			fmt.Println("Warning: couldn't get federated token, falling back to WAN")
			useLAN = false
		}
	}

	var allManifests []AnalysisManifest

	for _, camUUID := range cameraUUIDs {
		camName := cameraNameMap[camUUID]
		if camName == "" {
			camName = camUUID
		}
		fmt.Printf("\nProcessing: %s\n", camName)

		camDir := filepath.Join(outputDir, sanitizeName(camName))
		os.MkdirAll(camDir, 0755)

		// Get seekpoints for activity-aware frame selection
		activityTimes := getActivityTimes(cfg, camUUID, startMs, endMs, includeMotion)

		// Select frames: prioritize activity, optionally fill remainder evenly
		frameTimes := selectFrameTimes(startMs, endMs, interval, activityTimes, fill)
		activityCount := 0
		for _, ft := range frameTimes {
			if isActivityFrame(ft, activityTimes) {
				activityCount++
			}
		}
		fmt.Printf("  %d frames selected (%d activity, %d fill)\n",
			len(frameTimes), activityCount, len(frameTimes)-activityCount)

		// Download frames
		var frames []FrameEntry
		if useLAN && lanTemplates[camUUID] != "" {
			frames = downloadFramesLAN(cfg, camUUID, camDir, frameTimes, activityTimes, lanTemplates[camUUID], lanFedToken)
		} else {
			frames = downloadFramesWAN(cfg, camUUID, camDir, frameTimes, activityTimes)
		}
		fmt.Println()

		manifest := AnalysisManifest{
			Camera:    camName,
			CameraID:  camUUID,
			TimeRange: TimeRangeJSON{StartMs: startMs, EndMs: endMs},
			Frames:    frames,
		}
		allManifests = append(allManifests, manifest)

		manifestPath := filepath.Join(camDir, "manifest.json")
		manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
		os.WriteFile(manifestPath, manifestJSON, 0644)
	}

	// Write combined manifest
	combinedPath := filepath.Join(outputDir, "manifest.json")
	combinedJSON, _ := json.MarshalIndent(allManifests, "", "  ")
	os.WriteFile(combinedPath, combinedJSON, 0644)

	if raw {
		fmt.Printf("\nFrames extracted to: %s\n", outputDir)
		fmt.Printf("Manifest: %s\n", combinedPath)
		fmt.Println(string(combinedJSON))
		return nil
	}

	// Visual analysis for each camera
	for _, manifest := range allManifests {
		if err := visualAnalysis(manifest); err != nil {
			fmt.Fprintf(os.Stderr, "Analysis error for %s: %v\n", manifest.Camera, err)
		}
	}

	return nil
}

// ─── Frame Selection ────────────────────────────────────────────────

func getActivityTimes(cfg config.Config, cameraUUID string, startMs, endMs int64, includeMotion bool) []int64 {
	startSec := startMs / 1000
	durationSec := (endMs - startMs) / 1000

	params := map[string]any{
		"cameraUuid": cameraUUID,
		"startTime":  startSec,
		"duration":   durationSec,
	}
	if includeMotion {
		params["includeAnyMotion"] = "true"
	}

	resp, err := client.APICall(cfg, "/api/camera/getFootageSeekpointsV2", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Seekpoints fetch error: %v\n", err)
		return nil
	}

	seekpoints, _ := resp["footageSeekPoints"].([]any)
	fmt.Fprintf(os.Stderr, "  Seekpoints found: %d\n", len(seekpoints))
	var times []int64
	for _, sp := range seekpoints {
		s, ok := sp.(map[string]any)
		if !ok {
			continue
		}
		ts, _ := s["ts"].(float64)
		if ts > 0 {
			times = append(times, int64(ts))
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times
}

// downloadFramesWAN downloads frames via getExactFrameUri (WAN/cloud path).
func downloadFramesWAN(cfg config.Config, camUUID, camDir string, frameTimes, activityTimes []int64) []FrameEntry {
	var frames []FrameEntry
	for i, frameMs := range frameTimes {
		framePath := filepath.Join(camDir, fmt.Sprintf("frame_%03d.jpeg", i+1))

		frameResp, err := client.APICall(cfg, "/api/video/getExactFrameUri", map[string]any{
			"cameraUuid":  camUUID,
			"timestampMs": frameMs,
		})
		if err != nil {
			continue
		}

		frameUri, _ := frameResp["frameUri"].(string)
		if frameUri == "" {
			continue
		}

		frameUri = strings.Replace(frameUri, ".dash.rhombussystems.com", ".dash-internal.rhombussystems.com", 1)

		if err := downloadWithAuthQuiet(cfg, frameUri, framePath); err != nil {
			continue
		}

		frames = append(frames, FrameEntry{
			TimestampMs: frameMs,
			Path:        framePath,
			HasActivity: isActivityFrame(frameMs, activityTimes),
		})
		fmt.Printf("\r  Frames: %d/%d", len(frames), len(frameTimes))
	}
	return frames
}

// downloadFramesLAN downloads frames via LAN by fetching 2s VOD segments and extracting a frame.
// Each segment is 2 seconds with 1 keyframe, so we download the segment covering each timestamp
// and use ffmpeg to extract the nearest frame.
func downloadFramesLAN(cfg config.Config, camUUID, camDir string, frameTimes, activityTimes []int64, lanTemplate, fedToken string) []FrameEntry {
	httpClient, _ := client.GetMediaHTTPClient(cfg)
	setHeaders := func(req *http.Request) {
		req.Header.Set("Cookie", "RHOMBUS_SESSIONID=RFT:"+fedToken)
		req.Header.Set("x-auth-scheme", "api-token")
		req.Header.Set("x-auth-apikey", cfg.ApiKey)
	}

	segDir := filepath.Join(camDir, "segments")
	os.MkdirAll(segDir, 0755)
	defer os.RemoveAll(segDir)

	// Cache downloaded segments to avoid re-downloading the same 2s segment
	segCache := make(map[int64]string) // startSec → segment file path

	var frames []FrameEntry
	for i, frameMs := range frameTimes {
		framePath := filepath.Join(camDir, fmt.Sprintf("frame_%03d.jpeg", i+1))

		// Determine which 2s segment contains this frame
		frameSec := frameMs / 1000
		segStartSec := (frameSec / 2) * 2 // align to 2s boundary
		offsetSec := frameSec - segStartSec

		// Download segment if not cached
		segPath, ok := segCache[segStartSec]
		if !ok {
			segPath = filepath.Join(segDir, fmt.Sprintf("seg_%d.mp4", segStartSec))

			// Build LAN VOD URL for this 2s segment
			mpdURL := strings.Replace(lanTemplate, "{START_TIME}", fmt.Sprintf("%d", segStartSec), 1)
			mpdURL = strings.Replace(mpdURL, "{DURATION}", "2", 1)
			baseURL := mpdURL[:strings.LastIndex(mpdURL, "/")+1]

			// Download init + single segment, concatenate
			initPath := filepath.Join(segDir, fmt.Sprintf("init_%d.mp4", segStartSec))
			seg1Path := filepath.Join(segDir, fmt.Sprintf("data_%d.m4v", segStartSec))

			err1 := downloadDashSegment(httpClient, baseURL+"seg_init.mp4", initPath, setHeaders)
			err2 := downloadDashSegment(httpClient, baseURL+"seg_1.m4v", seg1Path, setHeaders)
			if err1 != nil || err2 != nil {
				continue
			}

			// Concatenate init + segment
			f, err := os.Create(segPath)
			if err != nil {
				continue
			}
			initData, _ := os.ReadFile(initPath)
			segData, _ := os.ReadFile(seg1Path)
			f.Write(initData)
			f.Write(segData)
			f.Close()

			// Clean up intermediate files
			os.Remove(initPath)
			os.Remove(seg1Path)

			segCache[segStartSec] = segPath
		}

		// Extract frame from segment using ffmpeg
		extractCmd := exec.Command("ffmpeg",
			"-i", segPath,
			"-ss", fmt.Sprintf("%d", offsetSec),
			"-frames:v", "1",
			"-q:v", "3",
			"-update", "1",
			framePath, "-y")
		extractCmd.Stderr = nil
		if err := extractCmd.Run(); err != nil {
			continue
		}

		frames = append(frames, FrameEntry{
			TimestampMs: frameMs,
			Path:        framePath,
			HasActivity: isActivityFrame(frameMs, activityTimes),
		})
		fmt.Printf("\r  Frames: %d/%d", len(frames), len(frameTimes))
	}
	return frames
}

// selectFrameTimes picks frames in two passes:
// 1. All activity frames (deduplicated to at most 1 per minFrameIntervalSec)
// 2. Remaining budget spread evenly across the window for coverage
func selectFrameTimes(startMs, endMs int64, intervalSec float64, activityTimes []int64, includeFill bool) []int64 {
	minIntervalMs := int64(minFrameIntervalSec * 1000)

	// Pass 1: Select activity frames, at most 1 per minFrameInterval
	var activityFrames []int64
	lastActivityFrame := int64(0)
	for _, at := range activityTimes {
		if at >= startMs && at <= endMs && (at-lastActivityFrame) >= minIntervalMs {
			activityFrames = append(activityFrames, at)
			lastActivityFrame = at
		}
	}

	if !includeFill {
		return activityFrames
	}

	// Pass 2: Fill remaining budget evenly across the window
	remaining := targetFramesPerCamera - len(activityFrames)
	if remaining <= 0 {
		return activityFrames
	}

	// Calculate interval for remaining frames
	fillInterval := float64(endMs-startMs) / float64(remaining+1)
	fillIntervalMs := int64(math.Max(float64(minIntervalMs), fillInterval))

	var fillFrames []int64
	for t := startMs + fillIntervalMs; t < endMs && len(fillFrames) < remaining; t += fillIntervalMs {
		// Skip if too close to an existing activity frame
		tooClose := false
		for _, af := range activityFrames {
			if abs64(t-af) < minIntervalMs {
				tooClose = true
				break
			}
		}
		if !tooClose {
			fillFrames = append(fillFrames, t)
		}
	}

	// Merge and sort all frames
	allFrames := append(activityFrames, fillFrames...)
	sort.Slice(allFrames, func(i, j int) bool { return allFrames[i] < allFrames[j] })
	return allFrames
}

func isActivityFrame(timestampMs int64, activityTimes []int64) bool {
	minIntervalMs := int64(minFrameIntervalSec * 1000)
	for _, at := range activityTimes {
		if abs64(at-timestampMs) <= minIntervalMs {
			return true
		}
		if at > timestampMs+minIntervalMs {
			break
		}
	}
	return false
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// ─── Visual Analysis ────────────────────────────────────────────────

func visualAnalysis(manifest AnalysisManifest) error {
	if len(manifest.Frames) == 0 {
		fmt.Printf("\n%s: No frames to analyze.\n", manifest.Camera)
		return nil
	}

	fmt.Printf("\n── %s (%d frames) ──\n", manifest.Camera, len(manifest.Frames))

	for _, frame := range manifest.Frames {
		ts := time.UnixMilli(frame.TimestampMs)
		activity := ""
		if frame.HasActivity {
			activity = " [ACTIVITY]"
		}
		fmt.Printf("  %s%s: %s\n", ts.Format("3:04:05 PM"), activity, frame.Path)
	}

	// Read frames for visual description
	fmt.Println("\nAnalyzing frames visually...")
	for i, frame := range manifest.Frames {
		if i > 20 {
			fmt.Printf("  ... (%d more frames in %s)\n", len(manifest.Frames)-20, filepath.Dir(frame.Path))
			break
		}
		// Read the image — Claude can see it when using the Read tool
		fmt.Printf("  Frame %d (%s): ", i+1, time.UnixMilli(frame.TimestampMs).Format("3:04:05 PM"))
		// For now, just note the file path — the calling agent reads the images
		fmt.Printf("%s\n", frame.Path)
	}

	return nil
}

// ─── Location Resolution ────────────────────────────────────────────

func resolveCamerasAtLocation(cfg config.Config, locationName string) ([]string, map[string]string, error) {
	// Get locations
	locResp, err := client.APICall(cfg, "/api/location/getLocations", map[string]any{})
	if err != nil {
		return nil, nil, fmt.Errorf("fetching locations: %w", err)
	}

	locations, _ := locResp["locations"].([]any)
	var locationUUID string
	for _, l := range locations {
		loc, ok := l.(map[string]any)
		if !ok {
			continue
		}
		name, _ := loc["name"].(string)
		uuid, _ := loc["uuid"].(string)
		if strings.Contains(strings.ToLower(name), strings.ToLower(locationName)) {
			locationUUID = uuid
			fmt.Printf("Location: %s (%s)\n", name, uuid)
			break
		}
	}
	if locationUUID == "" {
		return nil, nil, fmt.Errorf("no location matching \"%s\"", locationName)
	}

	// Get cameras and filter by location
	nameMap := getCameraNameMap(cfg)
	camResp, err := client.APICall(cfg, "/api/camera/getMinimalCameraStateList", map[string]any{})
	if err != nil {
		return nil, nil, err
	}

	cameras, _ := camResp["cameraStates"].([]any)
	var uuids []string
	for _, c := range cameras {
		cam, ok := c.(map[string]any)
		if !ok {
			continue
		}
		locUUID, _ := cam["locationUuid"].(string)
		camUUID, _ := cam["uuid"].(string)
		if locUUID == locationUUID && camUUID != "" {
			uuids = append(uuids, camUUID)
		}
	}

	return uuids, nameMap, nil
}

// ─── Natural Language Period Parsing ─────────────────────────────────

func parsePeriod(period string) (int64, int64, error) {
	period = strings.ToLower(strings.TrimSpace(period))
	now := time.Now()

	// Parse "yesterday between 8am and 9am"
	re1 := regexp.MustCompile(`(yesterday|today|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+(?:between\s+)?(\d{1,2}(?::\d{2})?\s*(?:am|pm)?)\s+(?:and|to|-)\s+(\d{1,2}(?::\d{2})?\s*(?:am|pm)?)`)
	if m := re1.FindStringSubmatch(period); m != nil {
		day := resolveDay(m[1], now)
		startTime := parseTimeOfDay(m[2])
		endTime := parseTimeOfDay(m[3])

		start := time.Date(day.Year(), day.Month(), day.Day(), startTime.hour, startTime.min, 0, 0, now.Location())
		end := time.Date(day.Year(), day.Month(), day.Day(), endTime.hour, endTime.min, 0, 0, now.Location())
		return start.UnixMilli(), end.UnixMilli(), nil
	}

	// Parse "last 2 hours"
	re2 := regexp.MustCompile(`last\s+(\d+)\s+(hour|minute|min|day)s?`)
	if m := re2.FindStringSubmatch(period); m != nil {
		n, _ := strconv.Atoi(m[1])
		var d time.Duration
		switch m[2] {
		case "hour":
			d = time.Duration(n) * time.Hour
		case "minute", "min":
			d = time.Duration(n) * time.Minute
		case "day":
			d = time.Duration(n) * 24 * time.Hour
		}
		return now.Add(-d).UnixMilli(), now.UnixMilli(), nil
	}

	// Parse "march 15 8am to 10am"
	re3 := regexp.MustCompile(`(january|february|march|april|may|june|july|august|september|october|november|december)\s+(\d{1,2})\s+(\d{1,2}(?::\d{2})?\s*(?:am|pm)?)\s+(?:to|-)\s+(\d{1,2}(?::\d{2})?\s*(?:am|pm)?)`)
	if m := re3.FindStringSubmatch(period); m != nil {
		month := parseMonth(m[1])
		day, _ := strconv.Atoi(m[2])
		startTime := parseTimeOfDay(m[3])
		endTime := parseTimeOfDay(m[4])

		start := time.Date(now.Year(), month, day, startTime.hour, startTime.min, 0, 0, now.Location())
		end := time.Date(now.Year(), month, day, endTime.hour, endTime.min, 0, 0, now.Location())
		return start.UnixMilli(), end.UnixMilli(), nil
	}

	return 0, 0, fmt.Errorf("could not parse period: \"%s\". Try: 'yesterday between 8am and 9am', 'last 2 hours', 'march 15 8am to 10am'")
}

type timeOfDay struct {
	hour, min int
}

func parseTimeOfDay(s string) timeOfDay {
	s = strings.TrimSpace(strings.ToLower(s))
	isPM := strings.Contains(s, "pm")
	isAM := strings.Contains(s, "am")
	s = strings.TrimRight(s, "apm ")

	parts := strings.Split(s, ":")
	hour, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	min := 0
	if len(parts) > 1 {
		min, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}

	if isPM && hour < 12 {
		hour += 12
	}
	if isAM && hour == 12 {
		hour = 0
	}

	return timeOfDay{hour, min}
}

func resolveDay(day string, now time.Time) time.Time {
	switch day {
	case "today":
		return now
	case "yesterday":
		return now.AddDate(0, 0, -1)
	default:
		// Day of week
		target := parseDayOfWeek(day)
		current := now.Weekday()
		diff := int(current) - int(target)
		if diff <= 0 {
			diff += 7
		}
		return now.AddDate(0, 0, -diff)
	}
}

func parseDayOfWeek(s string) time.Weekday {
	switch strings.ToLower(s) {
	case "sunday":
		return time.Sunday
	case "monday":
		return time.Monday
	case "tuesday":
		return time.Tuesday
	case "wednesday":
		return time.Wednesday
	case "thursday":
		return time.Thursday
	case "friday":
		return time.Friday
	case "saturday":
		return time.Saturday
	}
	return time.Sunday
}

func parseMonth(s string) time.Month {
	months := map[string]time.Month{
		"january": time.January, "february": time.February, "march": time.March,
		"april": time.April, "may": time.May, "june": time.June,
		"july": time.July, "august": time.August, "september": time.September,
		"october": time.October, "november": time.November, "december": time.December,
	}
	return months[strings.ToLower(s)]
}

// ─── Helpers ────────────────────────────────────────────────────────

func downloadAlertClipToFile(cfg config.Config, clipBaseURL, outputPath string) error {
	tmpDir, err := os.MkdirTemp("", "rhombus-clip-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	initPath := filepath.Join(tmpDir, "seg_init.mp4")
	if err := downloadWithAuthQuiet(cfg, clipBaseURL+"/seg_init.mp4", initPath); err != nil {
		return fmt.Errorf("init segment: %w", err)
	}

	var segPaths []string
	segPaths = append(segPaths, initPath)
	for i := 1; i <= 100; i++ {
		segPath := filepath.Join(tmpDir, fmt.Sprintf("seg_%d.m4v", i))
		if err := downloadWithAuthQuiet(cfg, fmt.Sprintf("%s/seg_%d.m4v", clipBaseURL, i), segPath); err != nil {
			break
		}
		segPaths = append(segPaths, segPath)
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	for _, segPath := range segPaths {
		data, _ := os.ReadFile(segPath)
		outFile.Write(data)
	}
	return nil
}

func downloadFrameQuiet(url, outputPath string) error {
	resp, err := http.Get(url)
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
	io.Copy(f, resp.Body)
	return nil
}

func dedup(times []float64, tolerance float64) []float64 {
	if len(times) == 0 {
		return times
	}
	sort.Float64s(times)
	result := []float64{times[0]}
	for i := 1; i < len(times); i++ {
		if times[i]-result[len(result)-1] >= tolerance {
			result = append(result, times[i])
		}
	}
	return result
}

func sanitizeName(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}
