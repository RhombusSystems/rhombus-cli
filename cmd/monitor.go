package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

const (
	defaultWSHost     = "ws.rhombussystems.com"
	defaultWSPort     = "8443"
	defaultWSPath     = "/websocket"
	stompVersion      = "1.2"
	heartbeatInterval = 10000 // milliseconds
)

func init() {
	monitorCmd := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor for alerts in real time via WebSocket",
		Long:  "Opens a WebSocket connection to Rhombus and streams policy alerts as they occur.",
		RunE:  runMonitor,
	}
	monitorCmd.Flags().Bool("all-events", false, "Show all change events, not just alerts")
	monitorCmd.Flags().Bool("json", false, "Output raw JSON payloads")
	rootCmd.AddCommand(monitorCmd)
}

func runMonitor(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	allEvents, _ := cmd.Flags().GetBool("all-events")
	jsonOutput, _ := cmd.Flags().GetBool("json")

	orgUuid, err := getOrgUuid(cfg)
	if err != nil {
		return fmt.Errorf("fetching org info: %w", err)
	}

	cameraNames := getCameraNameMap(cfg)

	fmt.Fprintf(os.Stderr, "Connecting to Rhombus WebSocket...\n")

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	wsURL := buildWSURL(cfg)
	headers := buildWSHeaders(cfg)

	var conn *websocket.Conn
	defer func() {
		if conn != nil {
			sendStompFrame(conn, "DISCONNECT", map[string]string{}, "")
			conn.Close()
		}
	}()

	topic := fmt.Sprintf("/topic/change/%s", orgUuid)

	for {
		conn, err = connectAndSubscribe(wsURL, headers, topic)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "Reconnecting in 5 seconds...\n")
			select {
			case <-sigCh:
				fmt.Fprintf(os.Stderr, "\nDisconnected.\n")
				return nil
			case <-time.After(5 * time.Second):
				continue
			}
		}

		fmt.Fprintf(os.Stderr, "Connected. Monitoring for alerts on org %s...\n", orgUuid)
		fmt.Fprintf(os.Stderr, "Press Ctrl+C to stop.\n\n")

		disconnected := handleMessages(conn, cfg, cameraNames, allEvents, jsonOutput, sigCh)
		conn.Close()
		conn = nil

		if !disconnected {
			fmt.Fprintf(os.Stderr, "\nDisconnected.\n")
			return nil
		}

		fmt.Fprintf(os.Stderr, "Connection lost. Reconnecting in 5 seconds...\n")
		select {
		case <-sigCh:
			fmt.Fprintf(os.Stderr, "\nDisconnected.\n")
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func getOrgUuid(cfg config.Config) (string, error) {
	resp, err := client.APICall(cfg, "/api/org/getOrgV2", map[string]any{})
	if err != nil {
		return "", err
	}
	org, ok := resp["org"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected response format")
	}
	uuid, ok := org["uuid"].(string)
	if !ok || uuid == "" {
		return "", fmt.Errorf("org UUID not found in response")
	}
	return uuid, nil
}

func buildWSURL(cfg config.Config) string {
	// Derive WS host from API endpoint
	host := defaultWSHost
	port := defaultWSPort

	if cfg.EndpointURL != "" && cfg.EndpointURL != config.DefaultEndpointURL {
		if u, err := url.Parse(cfg.EndpointURL); err == nil {
			// For custom endpoints (e.g. staging), derive WS host
			apiHost := u.Hostname()
			if strings.Contains(apiHost, "itg") {
				host = "ws.itg.rhombussystems.com"
			}
		}
	}

	// The websocket service only supports token-based API auth (api-token / partner-api-token),
	// not cert-based (api / partner-api). Pass x-auth-scheme as a query param so the server's
	// security filter chain matches the request (it checks both headers and query params).
	// The API key itself is sent as a header in buildWSHeaders.
	params := url.Values{}
	if cfg.IsPartner {
		params.Set("x-auth-scheme", "partner-api-token")
	} else {
		params.Set("x-auth-scheme", "api-token")
	}
	if cfg.PartnerOrg != "" {
		params.Set("x-auth-org", cfg.PartnerOrg)
	}

	return fmt.Sprintf("wss://%s:%s%s?%s", host, port, defaultWSPath, params.Encode())
}

func buildWSHeaders(cfg config.Config) http.Header {
	// The API key must be sent as a header (server reads x-auth-apikey from headers).
	// Use the token-based WS key if available, since the websocket service doesn't support cert auth.
	headers := http.Header{}
	apiKey := cfg.WSApiKey
	if apiKey == "" {
		apiKey = cfg.ApiKey
	}
	headers.Set("x-auth-apikey", apiKey)
	return headers
}

func connectAndSubscribe(wsURL string, headers http.Header, topic string) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("websocket dial: %w (HTTP %d: %s)", err, resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("websocket dial: %w", err)
	}

	// STOMP CONNECT
	err = sendStompFrame(conn, "CONNECT", map[string]string{
		"accept-version": stompVersion,
		"heart-beat":     fmt.Sprintf("%d,%d", heartbeatInterval, heartbeatInterval),
	}, "")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("STOMP CONNECT: %w", err)
	}

	// Read CONNECTED response
	frame, err := readStompFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading CONNECTED: %w", err)
	}
	if frame.command != "CONNECTED" {
		conn.Close()
		return nil, fmt.Errorf("expected CONNECTED, got %s", frame.command)
	}

	// SUBSCRIBE to change topic
	err = sendStompFrame(conn, "SUBSCRIBE", map[string]string{
		"id":          "sub-0",
		"destination": topic,
	}, "")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("STOMP SUBSCRIBE: %w", err)
	}

	return conn, nil
}

// handleMessages reads from the websocket and displays alerts.
// Returns true if disconnected unexpectedly (should reconnect), false if user cancelled.
func handleMessages(conn *websocket.Conn, cfg config.Config, cameraNames map[string]string, allEvents, jsonOutput bool, sigCh chan os.Signal) bool {
	msgCh := make(chan stompFrame, 16)
	errCh := make(chan error, 1)

	// Start heartbeat sender
	var wg sync.WaitGroup
	stopHeartbeat := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Duration(heartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				conn.WriteMessage(websocket.TextMessage, []byte("\n"))
			case <-stopHeartbeat:
				return
			}
		}
	}()

	// Start reader
	go func() {
		for {
			frame, err := readStompFrame(conn)
			if err != nil {
				errCh <- err
				return
			}
			if frame.command == "MESSAGE" {
				msgCh <- frame
			}
			// Heartbeat frames (empty command) are silently ignored
		}
	}()

	for {
		select {
		case <-sigCh:
			close(stopHeartbeat)
			wg.Wait()
			return false
		case err := <-errCh:
			close(stopHeartbeat)
			wg.Wait()
			_ = err
			return true
		case frame := <-msgCh:
			displayChangeEvent(frame, cfg, cameraNames, allEvents, jsonOutput)
		}
	}
}

func displayChangeEvent(frame stompFrame, cfg config.Config, cameraNames map[string]string, allEvents, jsonOutput bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(frame.body), &payload); err != nil {
		return
	}

	entity, _ := payload["entity"].(string)
	entityUuid, _ := payload["entityUuid"].(string)
	changeType, _ := payload["type"].(string)

	if !allEvents && entity != "POLICY_ALERT" {
		return
	}

	if jsonOutput {
		prettyJSON, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(prettyJSON))
		return
	}

	now := time.Now().Format("15:04:05")

	if entity == "POLICY_ALERT" {
		displayPolicyAlert(now, entityUuid, changeType, cfg, cameraNames)
	} else {
		// Generic event display for --all-events
		deviceUuid, _ := payload["deviceUuid"].(string)
		camName := ""
		if deviceUuid != "" {
			camName = cameraNames[deviceUuid]
			if camName == "" {
				camName = deviceUuid
			}
		}
		fmt.Printf("[%s] %s %s", now, changeType, entity)
		if camName != "" {
			fmt.Printf("  device=%s", camName)
		}
		if entityUuid != "" {
			fmt.Printf("  uuid=%s", entityUuid)
		}
		fmt.Println()
	}
}

func displayPolicyAlert(timestamp, alertUuid, changeType string, cfg config.Config, cameraNames map[string]string) {
	if changeType == "DELETE" {
		fmt.Printf("[%s] ALERT CLEARED  uuid=%s\n", timestamp, alertUuid)
		return
	}

	// Fetch alert details for a richer display
	alert, err := getAlertDetails(cfg, alertUuid)
	if err != nil {
		fmt.Printf("[%s] ALERT %s  uuid=%s\n", timestamp, changeType, alertUuid)
		return
	}

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

	alertTime := time.UnixMilli(int64(tsMs)).Format("3:04:05 PM")

	fmt.Printf("[%s] ALERT  camera=%-15s  time=%s  duration=%.0fs  triggers=%s",
		timestamp, camName, alertTime, durSec, strings.Join(triggerStrs, ", "))
	if description != "" {
		fmt.Printf("\n         %s", description)
	}
	fmt.Printf("\n         uuid=%s\n", alertUuid)
}

// STOMP protocol helpers

type stompFrame struct {
	command string
	headers map[string]string
	body    string
}

func sendStompFrame(conn *websocket.Conn, command string, headers map[string]string, body string) error {
	var b strings.Builder
	b.WriteString(command)
	b.WriteByte('\n')
	for k, v := range headers {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(body)
	b.WriteByte(0)
	return conn.WriteMessage(websocket.TextMessage, []byte(b.String()))
}

func readStompFrame(conn *websocket.Conn) (stompFrame, error) {
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return stompFrame{}, err
	}

	raw := string(msg)

	// Heartbeat frame (empty or just newlines)
	raw = strings.TrimLeft(raw, "\n\r")
	if raw == "" || raw == "\x00" {
		return stompFrame{}, nil
	}

	// Strip null terminator
	raw = strings.TrimRight(raw, "\x00")

	parts := strings.SplitN(raw, "\n\n", 2)
	headerSection := parts[0]
	body := ""
	if len(parts) > 1 {
		body = parts[1]
	}

	lines := strings.Split(headerSection, "\n")
	command := lines[0]
	headers := make(map[string]string)
	for _, line := range lines[1:] {
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			headers[line[:idx]] = line[idx+1:]
		}
	}

	return stompFrame{command: command, headers: headers, body: body}, nil
}
