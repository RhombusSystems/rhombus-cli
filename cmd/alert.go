package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

const mediaBaseURL = "https://media.rhombussystems.com"

func init() {
	alertCmd := &cobra.Command{
		Use:   "alert",
		Short: "View, download, and play policy alerts",
		Long:  "List policy alerts, download their thumbnails/clips, or play them in the browser.",
	}

	recentCmd := &cobra.Command{
		Use:   "recent",
		Short: "Show alerts from the last hour",
		RunE:  runAlertList,
	}
	recentCmd.Flags().String("camera", "", "Filter by camera name or UUID")
	recentCmd.Flags().String("after", "1h ago", "Show alerts after this time")
	recentCmd.Flags().Int("max", 20, "Maximum number of alerts to show")

	thumbCmd := &cobra.Command{
		Use:   "thumb [alert-uuid]",
		Short: "Download an alert's thumbnail",
		Args:  cobra.ExactArgs(1),
		RunE:  runAlertThumbnail,
	}
	thumbCmd.Flags().String("output", "", "Output file path (default: auto-generated)")

	downloadCmd := &cobra.Command{
		Use:   "download [alert-uuid]",
		Short: "Download an alert's video clip as MP4",
		Args:  cobra.ExactArgs(1),
		RunE:  runAlertDownload,
	}
	downloadCmd.Flags().String("output", "", "Output file path (default: auto-generated)")

	playCmd := &cobra.Command{
		Use:   "play [alert-uuid]",
		Short: "Play an alert's clip in the browser",
		Args:  cobra.ExactArgs(1),
		RunE:  runAlertPlay,
	}

	alertCmd.AddCommand(recentCmd)
	alertCmd.AddCommand(thumbCmd)
	alertCmd.AddCommand(downloadCmd)
	alertCmd.AddCommand(playCmd)
	rootCmd.AddCommand(alertCmd)
}

func runAlertList(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	cameraFilter, _ := cmd.Flags().GetString("camera")
	afterStr, _ := cmd.Flags().GetString("after")
	maxResults, _ := cmd.Flags().GetInt("max")

	afterMs, err := parseTimestamp(afterStr)
	if err != nil {
		return fmt.Errorf("invalid 'after' time: %w", err)
	}

	cameraNames := getCameraNameMap(cfg)

	var deviceFilter []string
	if cameraFilter != "" {
		uuid, _, err := resolveCamera(cfg, cameraFilter)
		if err != nil {
			return err
		}
		deviceFilter = []string{uuid}
	}

	body := map[string]any{
		"afterTimestampMs": afterMs,
		"maxResults":       maxResults,
	}
	if len(deviceFilter) > 0 {
		body["deviceFilter"] = deviceFilter
	}

	resp, err := client.APICall(cfg, "/api/event/getPolicyAlertsV2", body)
	if err != nil {
		return err
	}

	alerts, _ := resp["policyAlerts"].([]any)
	if len(alerts) == 0 {
		fmt.Println("No alerts found.")
		return nil
	}

	fmt.Printf("%-24s  %-15s  %-6s  %-20s  %s\n", "UUID", "Camera", "Dur", "Time", "Triggers")
	fmt.Println(strings.Repeat("-", 95))

	for _, a := range alerts {
		alert, ok := a.(map[string]any)
		if !ok {
			continue
		}

		uuid, _ := alert["uuid"].(string)
		deviceUuid, _ := alert["deviceUuid"].(string)
		tsMs, _ := alert["timestampMs"].(float64)
		durSec, _ := alert["durationSec"].(float64)
		triggers, _ := alert["policyAlertTriggers"].([]any)

		camName := cameraNames[deviceUuid]
		if camName == "" {
			camName = deviceUuid[:12] + "..."
		}
		if len(camName) > 15 {
			camName = camName[:15]
		}

		triggerStrs := make([]string, 0)
		for _, t := range triggers {
			if s, ok := t.(string); ok {
				triggerStrs = append(triggerStrs, s)
			}
		}

		ts := time.UnixMilli(int64(tsMs))
		fmt.Printf("%-24s  %-15s  %4.0fs  %-20s  %s\n",
			uuid, camName, durSec, ts.Format("Jan 2 3:04:05 PM"), strings.Join(triggerStrs, ", "))
	}

	return nil
}

func runAlertThumbnail(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	alertUuid := args[0]
	outputPath, _ := cmd.Flags().GetString("output")

	alert, err := getAlertDetails(cfg, alertUuid)
	if err != nil {
		return err
	}

	region := getAlertRegion(alert, "thumbnailLocation")
	thumbnailURL := fmt.Sprintf("%s/media/metadata/%s/%s.jpeg", mediaBaseURL, region, alertUuid)

	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("alert_%s.jpeg", alertUuid))
	}

	fmt.Printf("Downloading alert thumbnail...\n")
	if err := downloadWithAuth(cfg, thumbnailURL, outputPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	fmt.Printf("Thumbnail saved: %s\n", outputPath)
	openInBrowser("file://" + outputPath)
	return nil
}

func runAlertDownload(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	alertUuid := args[0]
	outputPath, _ := cmd.Flags().GetString("output")

	alert, err := getAlertDetails(cfg, alertUuid)
	if err != nil {
		return err
	}

	deviceUuid, _ := alert["deviceUuid"].(string)
	region := getAlertRegion(alert, "clipLocation")

	clipMpdURL := fmt.Sprintf("%s/media/metadata/%s/%s/%s/clip.mpd",
		mediaBaseURL, deviceUuid, region, alertUuid)

	if outputPath == "" {
		outputPath = fmt.Sprintf("alert_%s.mpd", alertUuid)
	}

	fmt.Printf("Downloading alert clip...\n")
	if err := downloadWithAuth(cfg, clipMpdURL, outputPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	fmt.Printf("Clip manifest saved: %s\n", outputPath)
	fmt.Println("Note: This is a DASH manifest. Use 'rhombus alert play' to view in browser.")
	return nil
}

func runAlertPlay(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	alertUuid := args[0]

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

	fmt.Printf("Playing alert: %s at %s (%.0fs)\n", camName,
		time.UnixMilli(int64(tsMs)).Format("Jan 2 3:04:05 PM"), durSec)

	serverURL, _, err := startPlayerServer(deviceUuid, camName, cfg, 3600)
	if err != nil {
		return fmt.Errorf("starting player: %w", err)
	}

	openInBrowser(serverURL)
	fmt.Println("Alert clip opened in browser.")
	fmt.Println("Press Ctrl+C to stop.")

	select {}
	return nil
}

func getAlertDetails(cfg config.Config, alertUuid string) (map[string]any, error) {
	resp, err := client.APICall(cfg, "/api/event/getPolicyAlertDetails", map[string]any{
		"policyAlertUuid": alertUuid,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching alert details: %w", err)
	}

	alert, ok := resp["policyAlert"].(map[string]any)
	if !ok || alert == nil {
		return nil, fmt.Errorf("alert not found: %s", alertUuid)
	}

	return alert, nil
}

func getAlertRegion(alert map[string]any, locationField string) string {
	loc, _ := alert[locationField].(map[string]any)
	if r, ok := loc["region"].(string); ok {
		return r
	}
	return "us-west-2"
}

func getCameraNameMap(cfg config.Config) map[string]string {
	names := make(map[string]string)
	resp, err := client.APICall(cfg, "/api/camera/getMinimalCameraStateList", map[string]any{})
	if err != nil {
		return names
	}
	cameras, _ := resp["cameraStates"].([]any)
	for _, c := range cameras {
		cam, ok := c.(map[string]any)
		if !ok {
			continue
		}
		uuid, _ := cam["uuid"].(string)
		name, _ := cam["name"].(string)
		if uuid != "" && name != "" {
			names[uuid] = name
		}
	}
	return names
}

func parseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "now" {
		return time.Now().UnixMilli(), nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms, nil
	}
	s = strings.TrimSuffix(s, " ago")
	s = strings.TrimSpace(s)
	for _, suffix := range []struct{ s string; d time.Duration }{
		{"s", time.Second}, {"m", time.Minute}, {"h", time.Hour},
	} {
		if strings.HasSuffix(s, suffix.s) {
			if n, err := strconv.Atoi(strings.TrimSuffix(s, suffix.s)); err == nil {
				return time.Now().Add(-time.Duration(n) * suffix.d).UnixMilli(), nil
			}
		}
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour).UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("cannot parse: %s (use epoch ms, 'now', or relative like '5m ago')", s)
}

func downloadWithAuth(cfg config.Config, url, outputPath string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-auth-apikey", cfg.ApiKey)
	if cfg.IsPartner {
		req.Header.Set("x-auth-scheme", "partner-api-token")
	} else {
		req.Header.Set("x-auth-scheme", "api-token")
	}

	httpClient, err := client.GetHTTPClient(cfg)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	dir := filepath.Dir(outputPath)
	if dir != "." && dir != "" {
		os.MkdirAll(dir, 0755)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	written, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}

	fmt.Printf("  Downloaded %.1f KB\n", float64(written)/1024)
	return nil
}

func marshalJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
