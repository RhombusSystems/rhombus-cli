package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

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

	fmt.Println("Rhombus MIND Chat")
	fmt.Println("Ask questions about your cameras, events, and devices. Type 'exit' to quit.")
	fmt.Println()

	// Use a stable context ID for this session so MIND has conversation history
	contextID := fmt.Sprintf("cli-%d", time.Now().UnixMilli())

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

		// Submit the query
		submitResp, err := client.APICall(cfg, "/api/chatbot/submitChat", map[string]any{
			"contextId": contextID,
			"query":     input,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
			continue
		}

		if isError(submitResp) {
			fmt.Fprintf(os.Stderr, "Error: %v\n\n", submitResp["errorMsg"])
			continue
		}

		recordUuid, _ := submitResp["chatRecordUuid"].(string)
		if recordUuid == "" {
			fmt.Fprintf(os.Stderr, "Error: no chat record UUID returned\n\n")
			continue
		}

		// Poll for the response
		fmt.Print("\033[1;32mmind>\033[0m \033[2mthinking...\033[0m")

		response, err := pollForResponse(cfg, recordUuid)
		// Clear the "thinking..." text
		fmt.Print("\r\033[K")

		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;32mmind>\033[0m Error: %v\n\n", err)
			continue
		}

		fmt.Printf("\033[1;32mmind>\033[0m %s\n\n", cleanResponse(response))
	}

	return nil
}

func pollForResponse(cfg config.Config, recordUuid string) (string, error) {
	maxAttempts := 120 // 2 minutes at 1 second intervals
	for i := 0; i < maxAttempts; i++ {
		time.Sleep(1 * time.Second)

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

		// Check if there's a response
		responseRaw, _ := chat["response"].(string)
		if responseRaw == "" {
			continue
		}

		// Response is JSON with textResponse field
		var responseObj map[string]any
		if err := json.Unmarshal([]byte(responseRaw), &responseObj); err == nil {
			if text, ok := responseObj["textResponse"].(string); ok && text != "" {
				return text, nil
			}
		}

		// Fallback: use raw response
		return responseRaw, nil

		// Still processing — update the spinner
		dots := strings.Repeat(".", (i%3)+1)
		fmt.Printf("\r\033[K\033[1;32mmind>\033[0m \033[2mthinking%s\033[0m", dots)
	}

	return "", fmt.Errorf("timed out waiting for response")
}

// cleanResponse strips console-internal routing links from MIND responses
var routePattern = regexp.MustCompile(`\[([^\]]+)\]\(\$ROUTE\([^)]+\)\$\)`)

func cleanResponse(text string) string {
	// Convert [Label]($ROUTE(...)$) → Label
	return routePattern.ReplaceAllString(text, "$1")
}

func isError(resp map[string]any) bool {
	if e, ok := resp["error"].(bool); ok && e {
		return true
	}
	return false
}
