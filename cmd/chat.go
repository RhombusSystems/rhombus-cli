package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

var toolDefinitions = []map[string]any{
	{
		"name":        "rhombus_cli",
		"description": `Execute a rhombus CLI command to query the Rhombus API. Do not include the 'rhombus' prefix.

Key commands:
- camera get-minimal-camera-state-list — list all cameras
- event get-policy-alerts-v2 --after-timestamp-ms <epoch_ms> — get recent alerts
- event get-policy-alert-details --policy-alert-uuid <uuid> — get alert details with seekpoints/bounding boxes
- alert recent --after "24h ago" — list recent policy alerts with camera names
- alert thumb <alert-uuid> — download and open alert thumbnail
- alert play <alert-uuid> — play alert clip in browser
- alert download <alert-uuid> — download alert clip
- live <camera-name> — open live video stream in browser
- video get-exact-frame-uri --camera-uuid <uuid> --timestamp-ms <ms> — get a frame URL
- access-control get-location-access-grants-by-org — get access control grants
- user get-users-in-org — list users
- sensor get-environmental-events — get sensor data

Use '--help' on any command to see available operations and '--generate-cli-skeleton' to discover parameters.`,
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The rhombus CLI command (without 'rhombus' prefix)",
				},
			},
			"required": []string{"command"},
		},
	},
}

func init() {
	chatCmd := &cobra.Command{
		Use:   "chat",
		Short: "Interactive AI chat powered by Rhombus MIND",
		Long:  "Start an interactive chat session with Rhombus MIND to query your cameras, events, and devices using natural language.",
		RunE:  runChat,
	}
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)
	chatProfile = cfg.Profile

	fmt.Println("Rhombus MIND Chat (powered by Claude)")
	fmt.Println("Ask questions about your cameras, events, and devices. Type 'exit' to quit.")
	fmt.Println()

	contextID := fmt.Sprintf("cli-%d", time.Now().UnixMilli())

	// Send tool definitions as the first message and wait for it to be processed
	fmt.Print("\033[2mInitializing tools...\033[0m")
	if err := sendToolDefinitions(cfg, contextID); err != nil {
		fmt.Printf("\r\033[K")
		fmt.Fprintf(os.Stderr, "Warning: failed to register tools: %v\n", err)
	} else {
		fmt.Printf("\r\033[K")
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for {
		fmt.Print("\033[1;34myou>\033[0m ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		response, err := submitAndWait(cfg, contextID, input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;32mmind>\033[0m Error: %v\n\n", err)
			continue
		}

		fmt.Printf("\033[1;32mmind>\033[0m %s\n\n", cleanResponse(response))
	}

	return nil
}

func sendToolDefinitions(cfg config.Config, contextID string) error {
	toolsJSON, err := json.Marshal(toolDefinitions)
	if err != nil {
		return err
	}

	query := "[TOOLS]" + string(toolsJSON)

	resp, err := client.APICall(cfg, "/api/chatbot/submitChat", map[string]any{
		"contextId": contextID,
		"query":     query,
	})
	if err != nil {
		return err
	}

	recordUuid, _ := resp["chatRecordUuid"].(string)
	if recordUuid == "" {
		return nil
	}

	// Wait for the tools message to be fully processed before continuing
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)

		pollResp, err := client.APICall(cfg, "/api/chatbot/getChatRecord", map[string]any{
			"recordUuid": recordUuid,
		})
		if err != nil {
			continue
		}

		chat, ok := pollResp["chat"].(map[string]any)
		if !ok {
			continue
		}

		// Any response means it's been processed
		if _, hasResponse := chat["response"]; hasResponse {
			return nil
		}

		timeline, _ := chat["timeline"].([]any)
		if len(timeline) > 0 {
			lastEvent, _ := timeline[len(timeline)-1].(map[string]any)
			status, _ := lastEvent["status"].(string)
			// If it reached any final status, it's done
			if status == "ANSWERED" || status == "NO_RESPONSE" || status == "PARTIALLY_ANSWERED" {
				return nil
			}
		}
	}

	return nil
}

// submitAndWait sends a query and polls for a response, handling tool call loops.
func submitAndWait(cfg config.Config, contextID, query string) (string, error) {
	submitResp, err := client.APICall(cfg, "/api/chatbot/submitChat", map[string]any{
		"contextId": contextID,
		"query":     query,
	})
	if err != nil {
		return "", err
	}
	if isError(submitResp) {
		return "", fmt.Errorf("%v", submitResp["errorMsg"])
	}

	recordUuid, _ := submitResp["chatRecordUuid"].(string)
	if recordUuid == "" {
		return "", fmt.Errorf("no chat record UUID returned")
	}

	fmt.Print("\033[1;32mmind>\033[0m \033[2mthinking...\033[0m")

	response, err := pollForResponse(cfg, recordUuid)
	fmt.Print("\r\033[K")

	if err != nil {
		return "", err
	}

	// Check if this is a tool_use response
	var responseObj map[string]any
	if err := json.Unmarshal([]byte(response), &responseObj); err == nil {
		if responseObj["responseType"] == "TOOL_USE" {
			return handleToolUse(cfg, contextID, responseObj)
		}
	}

	return response, nil
}

func handleToolUse(cfg config.Config, contextID string, responseObj map[string]any) (string, error) {
	toolCalls, _ := responseObj["toolCalls"].([]any)
	prefixText, _ := responseObj["textResponse"].(string)

	if prefixText != "" {
		fmt.Printf("\033[1;32mmind>\033[0m %s\n", cleanResponse(prefixText))
	}

	// Execute each tool call locally
	var results []string
	for _, tc := range toolCalls {
		toolCall, ok := tc.(map[string]any)
		if !ok {
			continue
		}

		name, _ := toolCall["name"].(string)
		id, _ := toolCall["id"].(string)
		input, _ := toolCall["input"].(map[string]any)

		if name != "rhombus_cli" {
			results = append(results, fmt.Sprintf("[tool_result id=%s] Unknown tool: %s", id, name))
			continue
		}

		command, _ := input["command"].(string)
		if command == "" {
			results = append(results, fmt.Sprintf("[tool_result id=%s] No command provided", id))
			continue
		}

		fmt.Fprintf(os.Stderr, "\033[2m  Running: rhombus %s\033[0m\n", command)

		output := executeRhombusCommand(command)

		// Truncate long output
		if len(output) > 30000 {
			output = output[:30000] + "\n... (truncated)"
		}

		results = append(results, fmt.Sprintf("[tool_result id=%s name=%s]\n%s", id, name, output))
	}

	// Send tool results back as a follow-up message
	toolResultQuery := strings.Join(results, "\n\n")
	return submitAndWait(cfg, contextID, toolResultQuery)
}

func pollForResponse(cfg config.Config, recordUuid string) (string, error) {
	maxAttempts := 240 // 2 minutes at 500ms intervals
	for i := 0; i < maxAttempts; i++ {
		time.Sleep(500 * time.Millisecond)

		resp, err := client.APICall(cfg, "/api/chatbot/getChatRecord", map[string]any{
			"recordUuid": recordUuid,
		})
		if err != nil {
			return "", fmt.Errorf("polling chat record: %w", err)
		}

		chat, ok := resp["chat"].(map[string]any)
		if !ok || chat == nil {
			continue
		}

		// Check timeline for failure statuses
		timeline, _ := chat["timeline"].([]any)
		if len(timeline) > 0 {
			lastEvent, _ := timeline[len(timeline)-1].(map[string]any)
			status, _ := lastEvent["status"].(string)

			switch status {
			case "NO_RESPONSE", "INVALID_REQUEST", "UNAUTHORIZED", "UNSUPPORTED",
				"DENIED", "NOT_UNDERSTOOD", "INTERRUPTED", "INVALID_AUTH_DATA",
				"INVALID_API_TOKEN", "MIND_DISABLED", "OUT_OF_TRIAL_CREDITS":
				desc, _ := lastEvent["description"].(string)
				if desc == "" {
					desc = status
				}
				return "", fmt.Errorf("%s", desc)

			case "AWAITING_TOOL_RESULTS", "CALLING_TOOLS":
				// Tool use response — return the raw response for the caller to parse
				responseRaw, _ := chat["response"].(string)
				if responseRaw != "" {
					return responseRaw, nil
				}
			}
		}

		// Check if there's a final response
		responseRaw, _ := chat["response"].(string)
		if responseRaw == "" {
			continue
		}

		// Check if it's answered (has a textResponse and timeline shows completion)
		if len(timeline) > 0 {
			lastEvent, _ := timeline[len(timeline)-1].(map[string]any)
			status, _ := lastEvent["status"].(string)
			if status == "ANSWERED" || status == "PARTIALLY_ANSWERED" ||
				status == "CLARIFICATION_REQUESTED" || status == "REDIRECTED" {
				// Extract text response
				var responseObj map[string]any
				if err := json.Unmarshal([]byte(responseRaw), &responseObj); err == nil {
					if text, ok := responseObj["textResponse"].(string); ok && text != "" {
						return text, nil
					}
				}
				return responseRaw, nil
			}
		}

		// Update spinner
		dots := strings.Repeat(".", (i%3)+1)
		fmt.Printf("\r\033[K\033[1;32mmind>\033[0m \033[2mthinking%s\033[0m", dots)
	}

	return "", fmt.Errorf("timed out waiting for response")
}

var chatProfile string

func executeRhombusCommand(command string) string {
	args := strings.Fields(command)

	executable, err := os.Executable()
	if err != nil {
		return "Error: could not find rhombus executable: " + err.Error()
	}

	args = append(args, "--output", "json")
	if chatProfile != "" {
		args = append(args, "--profile", chatProfile)
	}

	cmd := exec.Command(executable, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Command error: %s\nOutput: %s", err.Error(), string(output))
	}

	return string(output)
}

var routePattern = regexp.MustCompile(`\[([^\]]+)\]\(\$ROUTE\([^)]+\)\$\)`)

func cleanResponse(text string) string {
	return routePattern.ReplaceAllString(text, "$1")
}

func isError(resp map[string]any) bool {
	if e, ok := resp["error"].(bool); ok && e {
		return true
	}
	return false
}
