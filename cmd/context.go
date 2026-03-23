package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

type ContextManifest struct {
	GeneratedAt string            `json:"generatedAt"`
	Profile     string            `json:"profile"`
	OrgName     string            `json:"orgName,omitempty"`
	Locations   []ContextLocation `json:"locations"`
}

type ContextLocation struct {
	UUID    string          `json:"uuid"`
	Name    string          `json:"name"`
	Address string          `json:"address,omitempty"`
	TZ      string          `json:"tz,omitempty"`
	Lat     float64         `json:"latitude,omitempty"`
	Lon     float64         `json:"longitude,omitempty"`
	Cameras []ContextCamera `json:"cameras"`
}

type ContextCamera struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	HW           string   `json:"hw,omitempty"`
	Firmware     string   `json:"firmware,omitempty"`
	StillPath    string   `json:"stillPath,omitempty"`
	Lat          float64  `json:"latitude,omitempty"`
	Lon          float64  `json:"longitude,omitempty"`
	LANAddresses []string `json:"lanAddresses,omitempty"`
	Serial       string   `json:"serial,omitempty"`
}

func init() {
	contextCmd := &cobra.Command{
		Use:   "context",
		Short: "Generate and query deployment context for LLM-assisted analysis",
	}

	generateCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate a deployment context snapshot (locations, cameras, stills)",
		RunE:  runContextGenerate,
	}
	generateCmd.Flags().Bool("lan", false, "Prefer LAN for still downloads (faster on local network)")
	generateCmd.Flags().Int("concurrency", 4, "Number of parallel still downloads")

	locationCmd := &cobra.Command{
		Use:   "location [name]",
		Short: "Show detailed context for a location",
		Args:  cobra.ExactArgs(1),
		RunE:  runContextLocation,
	}

	cameraCmd := &cobra.Command{
		Use:   "camera [name-or-uuid]",
		Short: "Show detailed context for a camera with fresh still",
		Args:  cobra.ExactArgs(1),
		RunE:  runContextCamera,
	}

	contextCmd.AddCommand(generateCmd)
	contextCmd.AddCommand(locationCmd)
	contextCmd.AddCommand(cameraCmd)
	rootCmd.AddCommand(contextCmd)
}

func contextDir(profile string) string {
	return filepath.Join(rhombusDir(), "context", profile)
}

// ─── Generate ──────────────────────────────────────────────────────

func runContextGenerate(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")
	useLAN, _ := cmd.Flags().GetBool("lan")
	concurrency, _ := cmd.Flags().GetInt("concurrency")

	outDir := contextDir(profile)
	stillsDir := filepath.Join(outDir, "stills")
	os.MkdirAll(stillsDir, 0755)

	// Fetch locations
	fmt.Println("Fetching locations...")
	locations, err := fetchLocations(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("  %d locations\n", len(locations))

	// Fetch cameras
	fmt.Println("Fetching cameras...")
	cameras, err := fetchCameras(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("  %d cameras\n", len(cameras))

	// Map cameras to locations
	locMap := make(map[string]*ContextLocation)
	for i := range locations {
		locMap[locations[i].UUID] = &locations[i]
	}

	// Add an "Unassigned" location for cameras without one
	unassigned := ContextLocation{Name: "Unassigned"}
	for _, cam := range cameras {
		loc, ok := locMap[cam.locUUID]
		if !ok {
			unassigned.Cameras = append(unassigned.Cameras, cam.ContextCamera)
		} else {
			loc.Cameras = append(loc.Cameras, cam.ContextCamera)
		}
	}

	// Sort cameras within each location
	for i := range locations {
		sort.Slice(locations[i].Cameras, func(a, b int) bool {
			return locations[i].Cameras[a].Name < locations[i].Cameras[b].Name
		})
	}

	// Remove locations with no cameras, add unassigned if needed
	var finalLocations []ContextLocation
	for _, loc := range locations {
		if len(loc.Cameras) > 0 {
			finalLocations = append(finalLocations, loc)
		}
	}
	if len(unassigned.Cameras) > 0 {
		finalLocations = append(finalLocations, unassigned)
	}

	// LAN setup
	var lanTemplates map[string]string
	var fedToken string
	if useLAN {
		fmt.Println("Setting up LAN access...")
		lanTemplates = make(map[string]string)
		for _, cam := range cameras {
			if cam.Status != "GREEN" {
				continue
			}
			resp, err := client.APICall(cfg, "/api/camera/getMediaUris", map[string]any{
				"cameraUuid": cam.UUID,
			})
			if err != nil {
				continue
			}
			if templates, ok := resp["lanVodMpdUrisTemplates"].([]any); ok && len(templates) > 0 {
				if t, ok := templates[0].(string); ok {
					lanTemplates[cam.UUID] = t
				}
			}
		}
		fedResp, err := client.APICall(cfg, "/api/org/generateFederatedSessionToken", map[string]any{
			"durationSec": 3600,
		})
		if err == nil {
			fedToken, _ = fedResp["federatedSessionToken"].(string)
		}
		if fedToken != "" {
			fmt.Printf("  LAN ready: %d cameras\n", len(lanTemplates))
		} else {
			fmt.Println("  Warning: couldn't get federated token, using WAN")
			useLAN = false
		}
	}

	// Download stills with concurrency
	fmt.Println("Downloading stills...")
	type stillJob struct {
		camUUID string
		camName string
		outPath string
	}
	type stillResult struct {
		camUUID string
		outPath string
		err     error
	}

	jobs := make(chan stillJob, len(cameras))
	results := make(chan stillResult, len(cameras))
	var wg sync.WaitGroup

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var err error
				if useLAN && lanTemplates[job.camUUID] != "" {
					err = downloadStillLAN(cfg, lanTemplates[job.camUUID], fedToken, job.outPath)
				}
				if err != nil || !useLAN || lanTemplates[job.camUUID] == "" {
					err = downloadStillWAN(cfg, job.camUUID, job.outPath)
				}
				results <- stillResult{camUUID: job.camUUID, outPath: job.outPath, err: err}
			}
		}()
	}

	onlineCount := 0
	for _, cam := range cameras {
		if cam.Status != "GREEN" {
			continue
		}
		onlineCount++
		outPath := filepath.Join(stillsDir, sanitizeName(cam.Name)+".jpeg")
		jobs <- stillJob{camUUID: cam.UUID, camName: cam.Name, outPath: outPath}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	stillMap := make(map[string]string) // UUID → still path
	downloaded := 0
	failed := 0
	for r := range results {
		if r.err != nil {
			failed++
		} else {
			downloaded++
			stillMap[r.camUUID] = r.outPath
		}
		fmt.Printf("\r  Stills: %d/%d (failed: %d)", downloaded, onlineCount, failed)
	}
	fmt.Println()

	// Assign still paths back to cameras in locations
	for i := range finalLocations {
		for j := range finalLocations[i].Cameras {
			cam := &finalLocations[i].Cameras[j]
			if p, ok := stillMap[cam.UUID]; ok {
				cam.StillPath = p
			}
		}
	}

	// Build manifest
	manifest := ContextManifest{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		Profile:     profile,
		Locations:   finalLocations,
	}

	// Write manifest.json
	manifestPath := filepath.Join(outDir, "manifest.json")
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(manifestPath, manifestJSON, 0644)
	fmt.Printf("Manifest: %s\n", manifestPath)

	// Write index.md
	indexPath := filepath.Join(outDir, "index.md")
	if err := generateIndexMD(manifest, indexPath); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	fmt.Printf("Index: %s\n", indexPath)

	totalCams := 0
	for _, loc := range finalLocations {
		totalCams += len(loc.Cameras)
	}
	fmt.Printf("\nContext generated: %d locations, %d cameras, %d stills\n",
		len(finalLocations), totalCams, downloaded)

	return nil
}

func generateIndexMD(manifest ContextManifest, outputPath string) error {
	var b strings.Builder

	totalCams := 0
	for _, loc := range manifest.Locations {
		totalCams += len(loc.Cameras)
	}

	b.WriteString(fmt.Sprintf("# Deployment Context\n"))
	b.WriteString(fmt.Sprintf("Profile: %s | Generated: %s | Locations: %d | Cameras: %d\n\n",
		manifest.Profile, manifest.GeneratedAt, len(manifest.Locations), totalCams))

	for _, loc := range manifest.Locations {
		b.WriteString(fmt.Sprintf("## %s (%d cameras)\n", loc.Name, len(loc.Cameras)))
		if loc.Address != "" {
			b.WriteString(fmt.Sprintf("Address: %s", loc.Address))
			if loc.TZ != "" {
				b.WriteString(fmt.Sprintf(" | TZ: %s", loc.TZ))
			}
			b.WriteString("\n")
		}
		if loc.Lat != 0 || loc.Lon != 0 {
			b.WriteString(fmt.Sprintf("Coordinates: %.5f, %.5f\n", loc.Lat, loc.Lon))
		}

		for _, cam := range loc.Cameras {
			status := "ONLINE"
			if cam.Status != "GREEN" {
				status = "OFFLINE"
			}
			hw := cam.HW
			if hw != "" {
				hw = strings.TrimPrefix(hw, "CAMERA_")
			}

			line := fmt.Sprintf("- **%s** [%s]", cam.Name, status)
			if hw != "" {
				line += fmt.Sprintf(" — %s", hw)
			}
			if cam.StillPath != "" {
				line += fmt.Sprintf(" → [still](%s)", cam.StillPath)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	return os.WriteFile(outputPath, []byte(b.String()), 0644)
}

// ─── Location ──────────────────────────────────────────────────────

func runContextLocation(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")

	manifest, err := loadManifest(profile)
	if err != nil {
		return fmt.Errorf("no context found. Run 'rhombus context generate' first")
	}

	search := strings.ToLower(args[0])
	var match *ContextLocation
	for i := range manifest.Locations {
		if strings.Contains(strings.ToLower(manifest.Locations[i].Name), search) {
			match = &manifest.Locations[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf("no location matching %q", args[0])
	}

	fmt.Printf("## %s\n", match.Name)
	if match.Address != "" {
		fmt.Printf("Address: %s\n", match.Address)
	}
	if match.TZ != "" {
		fmt.Printf("Timezone: %s\n", match.TZ)
	}
	if match.Lat != 0 || match.Lon != 0 {
		fmt.Printf("Coordinates: %.5f, %.5f\n", match.Lat, match.Lon)
	}
	fmt.Printf("Cameras: %d\n\n", len(match.Cameras))

	for _, cam := range match.Cameras {
		status := "ONLINE"
		if cam.Status != "GREEN" {
			status = "OFFLINE"
		}
		hw := strings.TrimPrefix(cam.HW, "CAMERA_")
		fmt.Printf("  %-25s [%s] %s  %s\n", cam.Name, status, hw, cam.UUID)
		if cam.StillPath != "" {
			fmt.Printf("  %25s Still: %s\n", "", cam.StillPath)
		}
	}

	// Check freshness
	age := time.Since(parseContextTime(manifest.GeneratedAt))
	if age > 24*time.Hour {
		fmt.Fprintf(os.Stderr, "\nWarning: context is %.0f hours old. Run 'rhombus context generate' to refresh.\n", age.Hours())
	}

	// Provide the still paths for reading
	fmt.Println("\nTo view stills, read the image files listed above.")
	_ = cfg // used for partner-org resolution
	return nil
}

// ─── Camera ────────────────────────────────────────────────────────

func runContextCamera(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	profile, _ := cmd.Root().PersistentFlags().GetString("profile")

	camUUID, camName, err := resolveCamera(cfg, args[0])
	if err != nil {
		return err
	}

	fmt.Printf("## %s (%s)\n\n", camName, camUUID)

	// Download a fresh still
	outDir := contextDir(profile)
	stillsDir := filepath.Join(outDir, "stills")
	os.MkdirAll(stillsDir, 0755)
	stillPath := filepath.Join(stillsDir, sanitizeName(camName)+".jpeg")

	fmt.Println("Downloading fresh still...")
	if err := downloadStillWAN(cfg, camUUID, stillPath); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: couldn't download still: %v\n", err)
	} else {
		fmt.Printf("Still: %s\n", stillPath)
	}

	// Fetch full camera state
	state, err := client.APICall(cfg, "/api/camera/getFullCameraState", map[string]any{
		"cameraUuid": camUUID,
	})
	if err == nil {
		if cs, ok := state["fullCameraState"].(map[string]any); ok {
			fmt.Println()
			if hw, _ := cs["hwVariation"].(string); hw != "" {
				fmt.Printf("Hardware: %s\n", strings.TrimPrefix(hw, "CAMERA_"))
			}
			if fw, _ := cs["firmwareVersion"].(string); fw != "" {
				fmt.Printf("Firmware: %s\n", fw)
			}
			if serial, _ := cs["serialNumber"].(string); serial != "" {
				fmt.Printf("Serial: %s\n", serial)
			}
			if status, _ := cs["connectionStatus"].(string); status != "" {
				fmt.Printf("Status: %s\n", status)
			}
			if lat, ok := cs["latitude"].(float64); ok && lat != 0 {
				lon, _ := cs["longitude"].(float64)
				fmt.Printf("Coordinates: %.5f, %.5f\n", lat, lon)
			}
		}
	}

	// Look up location from manifest
	manifest, _ := loadManifest(profile)
	if manifest != nil {
		for _, loc := range manifest.Locations {
			for _, cam := range loc.Cameras {
				if cam.UUID == camUUID {
					fmt.Printf("Location: %s\n", loc.Name)
					if loc.Address != "" {
						fmt.Printf("Address: %s\n", loc.Address)
					}
					break
				}
			}
		}
	}

	// Recent activity summary
	nowMs := time.Now().UnixMilli()
	activity := getActivityTimes(cfg, camUUID, nowMs-3600*1000, nowMs, false)
	fmt.Printf("\nActivity (last hour): %d events\n", len(activity))

	return nil
}

// ─── Helpers ───────────────────────────────────────────────────────

type cameraWithLoc struct {
	ContextCamera
	locUUID string
}

func fetchLocations(cfg config.Config) ([]ContextLocation, error) {
	resp, err := client.APICall(cfg, "/api/location/getLocations", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("fetching locations: %w", err)
	}

	locs, _ := resp["locations"].([]any)
	var result []ContextLocation
	for _, l := range locs {
		m, ok := l.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		uuid, _ := m["uuid"].(string)
		addr := ""
		if a1, ok := m["address1"].(string); ok && a1 != "" {
			addr = a1
			if a2, ok := m["address2"].(string); ok && a2 != "" {
				addr += ", " + a2
			}
		}
		tz, _ := m["tz"].(string)
		lat, _ := m["latitude"].(float64)
		lon, _ := m["longitude"].(float64)

		result = append(result, ContextLocation{
			UUID:    uuid,
			Name:    name,
			Address: addr,
			TZ:      tz,
			Lat:     lat,
			Lon:     lon,
		})
	}
	return result, nil
}

func fetchCameras(cfg config.Config) ([]cameraWithLoc, error) {
	resp, err := client.APICall(cfg, "/api/camera/getMinimalCameraStateList", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("fetching cameras: %w", err)
	}

	cams, _ := resp["cameraStates"].([]any)
	var result []cameraWithLoc
	for _, c := range cams {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		uuid, _ := m["uuid"].(string)
		locUUID, _ := m["locationUuid"].(string)
		status, _ := m["connectionStatus"].(string)
		hw, _ := m["hwVariation"].(string)
		fw, _ := m["firmwareVersion"].(string)
		serial, _ := m["serialNumber"].(string)
		lat, _ := m["latitude"].(float64)
		lon, _ := m["longitude"].(float64)

		var lanAddrs []string
		if addrs, ok := m["lanAddresses"].([]any); ok {
			for _, a := range addrs {
				if s, ok := a.(string); ok {
					lanAddrs = append(lanAddrs, s)
				}
			}
		}

		result = append(result, cameraWithLoc{
			ContextCamera: ContextCamera{
				UUID:         uuid,
				Name:         name,
				Status:       status,
				HW:           hw,
				Firmware:     fw,
				Serial:       serial,
				Lat:          lat,
				Lon:          lon,
				LANAddresses: lanAddrs,
			},
			locUUID: locUUID,
		})
	}
	return result, nil
}

func downloadStillWAN(cfg config.Config, camUUID, outputPath string) error {
	nowMs := time.Now().UnixMilli()
	frameResp, err := client.APICall(cfg, "/api/video/getExactFrameUri", map[string]any{
		"cameraUuid":  camUUID,
		"timestampMs": nowMs,
	})
	if err != nil {
		return err
	}

	frameUri, _ := frameResp["frameUri"].(string)
	if frameUri == "" {
		return fmt.Errorf("no frame URI returned")
	}

	frameUri = strings.Replace(frameUri, ".dash.rhombussystems.com", ".dash-internal.rhombussystems.com", 1)
	return downloadWithAuthQuiet(cfg, frameUri, outputPath)
}

func downloadStillLAN(cfg config.Config, lanTemplate, fedToken, outputPath string) error {
	httpClient, _ := client.GetMediaHTTPClient(cfg)
	setHeaders := func(req *http.Request) {
		req.Header.Set("Cookie", "RHOMBUS_SESSIONID=RFT:"+fedToken)
		req.Header.Set("x-auth-scheme", "api-token")
		req.Header.Set("x-auth-apikey", cfg.ApiKey)
	}

	nowSec := time.Now().Unix()
	segStartSec := (nowSec / 2) * 2

	mpdURL := strings.Replace(lanTemplate, "{START_TIME}", fmt.Sprintf("%d", segStartSec), 1)
	mpdURL = strings.Replace(mpdURL, "{DURATION}", "2", 1)
	baseURL := mpdURL[:strings.LastIndex(mpdURL, "/")+1]

	tmpDir := outputPath + ".tmp"
	os.MkdirAll(tmpDir, 0755)
	defer os.RemoveAll(tmpDir)

	initPath := filepath.Join(tmpDir, "seg_init.mp4")
	seg1Path := filepath.Join(tmpDir, "seg_1.m4v")
	segPath := filepath.Join(tmpDir, "segment.mp4")

	if err := downloadDashSegment(httpClient, baseURL+"seg_init.mp4", initPath, setHeaders); err != nil {
		return fmt.Errorf("init segment: %w", err)
	}
	if err := downloadDashSegment(httpClient, baseURL+"seg_1.m4v", seg1Path, setHeaders); err != nil {
		return fmt.Errorf("media segment: %w", err)
	}

	// Concatenate init + segment
	f, err := os.Create(segPath)
	if err != nil {
		return err
	}
	initData, _ := os.ReadFile(initPath)
	segData, _ := os.ReadFile(seg1Path)
	f.Write(initData)
	f.Write(segData)
	f.Close()

	// Extract first frame
	extractCmd := exec.Command("ffmpeg",
		"-i", segPath,
		"-frames:v", "1",
		"-q:v", "3",
		"-update", "1",
		outputPath, "-y")
	extractCmd.Stderr = nil
	return extractCmd.Run()
}

func loadManifest(profile string) (*ContextManifest, error) {
	data, err := os.ReadFile(filepath.Join(contextDir(profile), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest ContextManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func parseContextTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t
}
