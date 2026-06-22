package cmd

import (
	"fmt"

	"github.com/RhombusSystems/rhombus-cli/internal/client"
	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/RhombusSystems/rhombus-cli/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	onguardCmd := &cobra.Command{
		Use:   "onguard",
		Short: "Query Honeywell OnGuard access-control events",
		Long:  "Search OnGuard badge events (grants and anomalies) recorded as camera seekpoints.",
	}

	eventsCmd := &cobra.Command{
		Use:   "events",
		Short: "Search OnGuard badge events",
		Long: "Search OnGuard access-control events by time, camera, location, cardholder, badge status, " +
			"area, and anomaly type. Scoped to your org and your accessible devices/locations. Emits JSON " +
			"by default for easy consumption by agents.\n\n" +
			"Examples:\n" +
			"  rhombus onguard events --after \"24h ago\"\n" +
			"  rhombus onguard events --cardholder \"Lisa Lake\" --after \"7d ago\"\n" +
			"  rhombus onguard events --anomaly-only --camera \"Lobby Cam\"",
		RunE: runOnGuardEvents,
	}
	eventsCmd.Flags().String("after", "24h ago", "Only events at/after this time (e.g. \"24h ago\", \"2026-06-16\")")
	eventsCmd.Flags().String("before", "", "Only events at/before this time")
	eventsCmd.Flags().String("camera", "", "Filter by camera name or UUID")
	eventsCmd.Flags().String("location", "", "Filter by location UUID")
	eventsCmd.Flags().String("cardholder", "", "Match the cardholder name (full-text)")
	eventsCmd.Flags().String("badge-status", "", "Filter by badge status (e.g. Active, Lost)")
	eventsCmd.Flags().String("badge-type", "", "Filter by badge type")
	eventsCmd.Flags().String("area", "", "Filter by the area entered (e.g. Lobby)")
	eventsCmd.Flags().Bool("anomaly-only", false, "Only anomaly events (inactive badge / no entry made)")
	eventsCmd.Flags().Int("max", 100, "Maximum number of events to return")

	onguardCmd.AddCommand(eventsCmd)
	rootCmd.AddCommand(onguardCmd)
}

func runOnGuardEvents(cmd *cobra.Command, args []string) error {
	cfg := config.LoadFromCmd(cmd)

	afterStr, _ := cmd.Flags().GetString("after")
	beforeStr, _ := cmd.Flags().GetString("before")
	cameraFilter, _ := cmd.Flags().GetString("camera")
	locationFilter, _ := cmd.Flags().GetString("location")
	cardholder, _ := cmd.Flags().GetString("cardholder")
	badgeStatus, _ := cmd.Flags().GetString("badge-status")
	badgeType, _ := cmd.Flags().GetString("badge-type")
	area, _ := cmd.Flags().GetString("area")
	anomalyOnly, _ := cmd.Flags().GetBool("anomaly-only")
	maxResults, _ := cmd.Flags().GetInt("max")

	body := map[string]any{
		"limit": maxResults,
		// Scope the unified integration access-event search to OnGuard's dedicated activity types.
		"activityTypes": []string{
			"ONGUARD_BADGE_AUTHORIZED",
			"ONGUARD_BADGE_ANOMALY",
			"ONGUARD_NO_ENTRY_MADE",
		},
	}

	if afterStr != "" {
		afterMs, err := parseTimestamp(afterStr)
		if err != nil {
			return fmt.Errorf("invalid 'after' time: %w", err)
		}
		body["afterMs"] = afterMs
	}
	if beforeStr != "" {
		beforeMs, err := parseTimestamp(beforeStr)
		if err != nil {
			return fmt.Errorf("invalid 'before' time: %w", err)
		}
		body["beforeMs"] = beforeMs
	}
	if cameraFilter != "" {
		uuid, _, err := resolveCamera(cfg, cameraFilter)
		if err != nil {
			return err
		}
		body["deviceUuids"] = []string{uuid}
	}
	if locationFilter != "" {
		body["locationUuids"] = []string{locationFilter}
	}
	if cardholder != "" {
		body["cardholderQuery"] = cardholder
	}
	if badgeStatus != "" {
		body["badgeStatus"] = badgeStatus
	}
	if badgeType != "" {
		body["badgeType"] = badgeType
	}
	if area != "" {
		body["area"] = area
	}
	if anomalyOnly {
		body["anomalyOnly"] = true
	}

	resp, err := client.APICall(cfg, "/api/eventSearchV2/searchIntegrationAccessEvents", body)
	if err != nil {
		return err
	}

	return output.FormatOutput(cmd, resp)
}
