package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
)

func init() {
	rootCmd.AddCommand(configureCmd)
}

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Configure Rhombus CLI credentials and settings",
	RunE: func(cmd *cobra.Command, args []string) error {
		profile, _ := cmd.Root().PersistentFlags().GetString("profile")
		if profile == "" {
			profile = config.DefaultProfile
		}

		// Load existing config to show current values
		existing := config.LoadConfig(profile)

		reader := bufio.NewReader(os.Stdin)

		apiKey := prompt(reader, "Rhombus API Key", maskKey(existing.ApiKey))
		outputFmt := prompt(reader, "Default output format", existing.Output)
		endpoint := prompt(reader, "Default endpoint URL", existing.EndpointURL)

		if apiKey != "" {
			if err := config.SaveCredentials(profile, apiKey); err != nil {
				return fmt.Errorf("failed to save credentials: %w", err)
			}
		}

		if err := config.SaveConfig(profile, outputFmt, endpoint); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Println("Configuration saved.")
		return nil
	},
}

func prompt(reader *bufio.Reader, label, current string) string {
	if current != "" {
		fmt.Printf("%s [%s]: ", label, current)
	} else {
		fmt.Printf("%s [None]: ", label)
	}
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return current
	}
	return input
}

func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}
