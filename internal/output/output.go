package output

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func FormatOutput(cmd *cobra.Command, data any) error {
	format, _ := cmd.Root().PersistentFlags().GetString("output")
	if format == "" {
		format = "json"
	}

	switch format {
	case "json":
		return printJSON(data)
	case "table":
		// Phase 2: table output
		fmt.Fprintln(os.Stderr, "Table output not yet implemented, falling back to JSON")
		return printJSON(data)
	case "text":
		// Phase 2: text output
		fmt.Fprintln(os.Stderr, "Text output not yet implemented, falling back to JSON")
		return printJSON(data)
	default:
		return fmt.Errorf("unknown output format: %s", format)
	}
}

func printJSON(data any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "    ")
	return enc.Encode(data)
}
