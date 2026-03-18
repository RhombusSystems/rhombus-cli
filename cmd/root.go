package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/RhombusSystems/rhombus-cli/cmd/generated"
	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "rhombus",
	Short: "CLI for the Rhombus API",
	Long:  "A command-line interface for all Rhombus API operations.",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Skip partner org resolution for commands that don't make API calls
		name := cmd.Name()
		if name == "login" || name == "configure" || name == "help" || name == "completion" {
			return nil
		}
		return resolvePartnerOrg(cmd)
	},
}

func init() {
	rootCmd.PersistentFlags().String("profile", "default", "Configuration profile to use")
	rootCmd.PersistentFlags().String("output", "", "Output format: json, table, text")
	rootCmd.PersistentFlags().String("api-key", "", "Override API key")
	rootCmd.PersistentFlags().String("endpoint-url", "", "Override endpoint URL")
	rootCmd.PersistentFlags().String("partner-org", "", "Client org name or UUID for partner API calls")

	generated.RegisterAll(rootCmd)
}

func SetVersion(v string) {
	rootCmd.Version = v
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolvePartnerOrg checks if --partner-org is a name (not a UUID) and resolves it
// by fetching partner clients and doing a case-insensitive substring match.
func resolvePartnerOrg(cmd *cobra.Command) error {
	partnerOrg, _ := cmd.Root().PersistentFlags().GetString("partner-org")
	if partnerOrg == "" {
		return nil
	}

	// If it looks like a base64 UUID (22 chars, alphanumeric + -_), assume it's already a UUID
	if looksLikeUUID(partnerOrg) {
		return nil
	}

	// It's a name search — fetch partner clients
	cfg := config.LoadFromCmd(cmd)
	if !cfg.IsPartner {
		return fmt.Errorf("--partner-org name search requires a partner profile. Use a UUID or run 'rhombus login' with a partner account")
	}

	result, err := client.APICall(cfg, "/api/partner/getPartnerClientsV2", map[string]any{})
	if err != nil {
		return fmt.Errorf("fetching partner clients: %w", err)
	}

	clients, ok := result["partnerClients"].([]any)
	if !ok || len(clients) == 0 {
		return fmt.Errorf("no partner clients found")
	}

	// Case-insensitive substring match
	search := strings.ToLower(partnerOrg)
	var matches []struct {
		name    string
		orgUuid string
	}

	for _, c := range clients {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		orgUuid, _ := m["orgUuid"].(string)
		if name == "" || orgUuid == "" {
			continue
		}
		if strings.Contains(strings.ToLower(name), search) {
			matches = append(matches, struct {
				name    string
				orgUuid string
			}{name, orgUuid})
		}
	}

	if len(matches) == 0 {
		return fmt.Errorf("no client orgs matching \"%s\"", partnerOrg)
	}

	if len(matches) == 1 {
		fmt.Fprintf(os.Stderr, "Using client org: %s (%s)\n", matches[0].name, matches[0].orgUuid)
		cmd.Root().PersistentFlags().Set("partner-org", matches[0].orgUuid)
		return nil
	}

	// Multiple matches — prompt user to pick
	fmt.Fprintf(os.Stderr, "Multiple orgs match \"%s\":\n", partnerOrg)
	for i, m := range matches {
		fmt.Fprintf(os.Stderr, "  [%d] %s (%s)\n", i+1, m.name, m.orgUuid)
	}
	fmt.Fprintf(os.Stderr, "Select [1-%d]: ", len(matches))

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	var choice int
	if _, err := fmt.Sscanf(input, "%d", &choice); err != nil || choice < 1 || choice > len(matches) {
		return fmt.Errorf("invalid selection")
	}

	selected := matches[choice-1]
	fmt.Fprintf(os.Stderr, "Using client org: %s (%s)\n", selected.name, selected.orgUuid)
	cmd.Root().PersistentFlags().Set("partner-org", selected.orgUuid)
	return nil
}

func looksLikeUUID(s string) bool {
	if len(s) != 22 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
