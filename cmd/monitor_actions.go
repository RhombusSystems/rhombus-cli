package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

// monitorOpts holds parsed flag values for the monitor action pipeline.
type monitorOpts struct {
	Download           bool
	OutputDir          string
	AISummary          bool
	AnthropicAPIKey    string
	OnAlert            string
	WebhookURL         string
	SummaryInterval    time.Duration
	AIAggregateSummary bool
}

func (o monitorOpts) hasActions() bool {
	return o.Download || o.AISummary || o.OnAlert != "" || o.WebhookURL != "" || o.SummaryInterval > 0
}

// alertEvent is the enriched alert passed through the action pipeline.
type alertEvent struct {
	UUID          string
	Alert         map[string]any
	CameraName    string
	Timestamp     time.Time
	Duration      float64
	Triggers      []string
	Description   string
	ThumbnailPath string
	ClipPath      string
	AISummaryText string
}

// parseMonitorOpts reads flag values from the command.
func parseMonitorOpts(cmd *cobra.Command) (monitorOpts, error) {
	var opts monitorOpts
	var err error

	opts.Download, _ = cmd.Flags().GetBool("download")
	opts.OutputDir, _ = cmd.Flags().GetString("output-dir")
	opts.AISummary, _ = cmd.Flags().GetBool("ai-summary")
	opts.AnthropicAPIKey, _ = cmd.Flags().GetString("anthropic-api-key")
	opts.OnAlert, _ = cmd.Flags().GetString("on-alert")
	opts.WebhookURL, _ = cmd.Flags().GetString("webhook")
	opts.SummaryInterval, _ = cmd.Flags().GetDuration("summary-interval")
	opts.AIAggregateSummary, _ = cmd.Flags().GetBool("ai-aggregate-summary")

	// Default output dir
	if opts.OutputDir == "" {
		home, _ := os.UserHomeDir()
		opts.OutputDir = filepath.Join(home, ".rhombus", "alerts")
	}

	// Resolve Anthropic API key
	if opts.AnthropicAPIKey == "" {
		opts.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	// Validate: AI features require an API key
	if (opts.AISummary || opts.AIAggregateSummary) && opts.AnthropicAPIKey == "" {
		return opts, fmt.Errorf("--ai-summary or --ai-aggregate-summary requires --anthropic-api-key or ANTHROPIC_API_KEY env var")
	}

	// AI summary requires download (needs the thumbnail)
	if opts.AISummary {
		opts.Download = true
	}

	_ = err
	return opts, nil
}

// ─── Alert Pipeline ────────────────────────────────────────────────

// processAlert runs the action pipeline for a single alert in a goroutine.
// The fetchedAlert contains already-retrieved alert details (no redundant API call).
func processAlert(ctx context.Context, cfg config.Config, cameraNames map[string]string, opts monitorOpts, agg *alertAggregator, fa *fetchedAlert) {
	// Build alertEvent from the already-fetched data
	evt := buildAlertEvent(fa.UUID, fa.Alert, cameraNames)

	// 1. Download media
	if opts.Download {
		evt.ThumbnailPath, evt.ClipPath = downloadAlertMedia(ctx, cfg, evt, opts.OutputDir)
		if evt.ThumbnailPath != "" || evt.ClipPath != "" {
			fmt.Fprintf(os.Stderr, "  [action] Downloaded: thumb=%s clip=%s\n", evt.ThumbnailPath, evt.ClipPath)
		}
	}

	// 2. AI Summary
	if opts.AISummary && evt.ThumbnailPath != "" {
		evt.AISummaryText = generateAISummary(ctx, evt, opts.AnthropicAPIKey)
		if evt.AISummaryText != "" {
			fmt.Fprintf(os.Stderr, "  [action] AI Summary: %s\n", evt.AISummaryText)
		}
	}

	// 3. Callback
	if opts.OnAlert != "" {
		executeAlertCallback(ctx, opts.OnAlert, evt)
	}

	// 4. Webhook
	if opts.WebhookURL != "" {
		postAlertWebhook(ctx, opts.WebhookURL, evt)
	}

	// 5. Aggregator
	if agg != nil {
		agg.add(evt)
	}
}

func buildAlertEvent(uuid string, alert map[string]any, cameraNames map[string]string) alertEvent {
	deviceUuid, _ := alert["deviceUuid"].(string)
	tsMs, _ := alert["timestampMs"].(float64)
	durSec, _ := alert["durationSec"].(float64)
	triggers, _ := alert["policyAlertTriggers"].([]any)
	description, _ := alert["textDescription"].(string)

	camName := cameraNames[deviceUuid]
	if camName == "" && deviceUuid != "" {
		if len(deviceUuid) > 12 {
			camName = deviceUuid[:12] + "..."
		} else {
			camName = deviceUuid
		}
	}

	triggerStrs := make([]string, 0, len(triggers))
	for _, t := range triggers {
		if s, ok := t.(string); ok {
			triggerStrs = append(triggerStrs, s)
		}
	}

	return alertEvent{
		UUID:        uuid,
		Alert:       alert,
		CameraName:  camName,
		Timestamp:   time.UnixMilli(int64(tsMs)),
		Duration:    durSec,
		Triggers:    triggerStrs,
		Description: description,
	}
}

// ─── Download ──────────────────────────────────────────────────────

func downloadAlertMedia(ctx context.Context, cfg config.Config, evt alertEvent, outputDir string) (thumbPath, clipPath string) {
	alertDir := filepath.Join(outputDir, evt.Timestamp.Format("2006-01-02"), evt.UUID)
	os.MkdirAll(alertDir, 0755)

	// Thumbnail
	region := getAlertRegion(evt.Alert, "thumbnailLocation")
	thumbURL := fmt.Sprintf("%s/media/metadata/%s/%s.jpeg", mediaBaseURL, region, evt.UUID)
	thumbPath = filepath.Join(alertDir, "thumbnail.jpeg")
	if err := downloadWithAuthQuiet(cfg, thumbURL, thumbPath); err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Thumbnail download failed: %v\n", err)
		thumbPath = ""
	}

	// Clip
	deviceUuid, _ := evt.Alert["deviceUuid"].(string)
	clipRegion := getAlertRegion(evt.Alert, "clipLocation")
	clipBaseURL := fmt.Sprintf("%s/media/metadata/%s/%s/%s", mediaBaseURL, deviceUuid, clipRegion, evt.UUID)
	clipPath = filepath.Join(alertDir, "clip.mp4")
	if err := downloadAlertClipToFile(cfg, clipBaseURL, clipPath); err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Clip download failed: %v\n", err)
		clipPath = ""
	}

	return thumbPath, clipPath
}

// ─── AI Summary ────────────────────────────────────────────────────

func generateAISummary(ctx context.Context, evt alertEvent, apiKey string) string {
	imgData, err := os.ReadFile(evt.ThumbnailPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Reading thumbnail: %v\n", err)
		return ""
	}
	b64Image := base64.StdEncoding.EncodeToString(imgData)

	prompt := fmt.Sprintf(
		"This is a security camera alert thumbnail from camera '%s'. "+
			"Triggers: %s. Duration: %.0fs. Alert description: %s. "+
			"Describe what you see in 1-2 concise sentences focused on the security-relevant activity.",
		evt.CameraName, strings.Join(evt.Triggers, ", "),
		evt.Duration, evt.Description)

	reqBody := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 150,
		"messages": []map[string]any{{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/jpeg",
						"data":       b64Image,
					},
				},
				map[string]any{
					"type": "text",
					"text": prompt,
				},
			},
		}},
	}

	jsonBody, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [action] AI API error: %v\n", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "  [action] AI API HTTP %d: %s\n", resp.StatusCode, string(body))
		return ""
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// ─── Callback ──────────────────────────────────────────────────────

func executeAlertCallback(ctx context.Context, command string, evt alertEvent) {
	payload := buildAlertPayload(evt)
	jsonData, _ := json.Marshal(payload)

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(jsonData)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Callback error: %v\n", err)
	}
}

// ─── Webhook ───────────────────────────────────────────────────────

func postAlertWebhook(ctx context.Context, webhookURL string, evt alertEvent) {
	payload := buildAlertPayload(evt)
	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(jsonData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Webhook request error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Webhook error: %v\n", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "  [action] Webhook HTTP %d\n", resp.StatusCode)
	}
}

func buildAlertPayload(evt alertEvent) map[string]any {
	return map[string]any{
		"uuid":          evt.UUID,
		"cameraName":    evt.CameraName,
		"timestamp":     evt.Timestamp.Unix(),
		"timestampMs":   evt.Timestamp.UnixMilli(),
		"duration":      evt.Duration,
		"triggers":      evt.Triggers,
		"description":   evt.Description,
		"thumbnailPath": evt.ThumbnailPath,
		"clipPath":      evt.ClipPath,
		"aiSummary":     evt.AISummaryText,
	}
}

// ─── Aggregator ────────────────────────────────────────────────────

type alertAggregator struct {
	mu       sync.Mutex
	alerts   []alertEvent
	interval time.Duration
	opts     monitorOpts
	done     chan struct{}
}

func newAlertAggregator(interval time.Duration, opts monitorOpts) *alertAggregator {
	return &alertAggregator{
		interval: interval,
		opts:     opts,
		done:     make(chan struct{}),
	}
}

func (a *alertAggregator) add(evt alertEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, evt)
}

func (a *alertAggregator) run() {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.flush()
		case <-a.done:
			a.flush() // Final flush on shutdown
			return
		}
	}
}

func (a *alertAggregator) stop() {
	close(a.done)
}

func (a *alertAggregator) flush() {
	a.mu.Lock()
	alerts := a.alerts
	a.alerts = nil
	a.mu.Unlock()

	if len(alerts) == 0 {
		return
	}

	summary := buildStructuredSummary(alerts, a.interval)
	fmt.Fprintf(os.Stderr, "\n%s", summary)

	if a.opts.AIAggregateSummary && a.opts.AnthropicAPIKey != "" {
		aiSummary := generateAggregateSummary(alerts, a.opts.AnthropicAPIKey)
		if aiSummary != "" {
			fmt.Fprintf(os.Stderr, "  AI Analysis: %s\n", aiSummary)
		}
	}
	fmt.Fprintln(os.Stderr)
}

func buildStructuredSummary(alerts []alertEvent, interval time.Duration) string {
	byCam := make(map[string]int)
	byTrigger := make(map[string]int)
	for _, a := range alerts {
		byCam[a.CameraName]++
		for _, t := range a.Triggers {
			byTrigger[t]++
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("── Summary (%d alerts in last %s) ──\n", len(alerts), interval))
	b.WriteString("  By camera:\n")
	for cam, count := range byCam {
		b.WriteString(fmt.Sprintf("    %-25s %d alerts\n", cam, count))
	}
	if len(byTrigger) > 0 {
		b.WriteString("  By trigger:\n")
		for trigger, count := range byTrigger {
			b.WriteString(fmt.Sprintf("    %-25s %d\n", trigger, count))
		}
	}

	// List per-alert AI summaries if available
	hasAI := false
	for _, a := range alerts {
		if a.AISummaryText != "" {
			hasAI = true
			break
		}
	}
	if hasAI {
		b.WriteString("  Descriptions:\n")
		for _, a := range alerts {
			if a.AISummaryText != "" {
				b.WriteString(fmt.Sprintf("    [%s] %s: %s\n",
					a.Timestamp.Format("15:04:05"), a.CameraName, a.AISummaryText))
			}
		}
	}

	return b.String()
}

func generateAggregateSummary(alerts []alertEvent, apiKey string) string {
	// Build a text summary of all alerts for AI analysis
	var details strings.Builder
	for _, a := range alerts {
		details.WriteString(fmt.Sprintf("- %s at %s: triggers=%s, duration=%.0fs",
			a.CameraName, a.Timestamp.Format("3:04:05 PM"),
			strings.Join(a.Triggers, "/"), a.Duration))
		if a.Description != "" {
			details.WriteString(fmt.Sprintf(", desc=%s", a.Description))
		}
		if a.AISummaryText != "" {
			details.WriteString(fmt.Sprintf(", visual=%s", a.AISummaryText))
		}
		details.WriteString("\n")
	}

	prompt := fmt.Sprintf(
		"You are a security operations analyst. Analyze these %d security camera alerts from the last period "+
			"and provide a brief 2-3 sentence summary highlighting any patterns, areas of concern, "+
			"or notable activity:\n\n%s", len(alerts), details.String())

	reqBody := map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 200,
		"messages": []map[string]any{{
			"role":    "user",
			"content": prompt,
		}},
	}

	jsonBody, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [action] Aggregate AI error: %v\n", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "  [action] Aggregate AI HTTP %d: %s\n", resp.StatusCode, string(body))
		return ""
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}
