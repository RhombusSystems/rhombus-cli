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
		outputFmt := orDefault(prompt(reader, "Default output format", existing.Output), existing.Output)

		currentRegion := config.RegionForEndpoint(existing.EndpointURL)
		if currentRegion == "" {
			currentRegion = config.RegionUS
		}
		region := strings.ToLower(orDefault(prompt(reader, "Region (us/eu)", currentRegion), currentRegion))

		endpointDefault := config.EndpointForRegion(region)
		if region == "" || (region != config.RegionUS && region != config.RegionEU) {
			// Unknown/custom region — keep the existing endpoint as default so the
			// user can type any URL.
			endpointDefault = existing.EndpointURL
		}
		endpoint := orDefault(prompt(reader, "Default endpoint URL", endpointDefault), endpointDefault)

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

// BE - returns "" when the default is accepted; the displayed value may be a mask, so callers must never persist it.
func prompt(reader *bufio.Reader, label, display string) string {
	if display != "" {
		fmt.Printf("%s [%s]: ", label, display)
	} else {
		fmt.Printf("%s [None]: ", label)
	}
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(input)
}

func orDefault(input, fallback string) string {
	if input == "" {
		return fallback
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
