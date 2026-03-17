package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/RhombusSystems/rhombus-cli/cmd/generated"
)

var rootCmd = &cobra.Command{
	Use:   "rhombus",
	Short: "CLI for the Rhombus API",
	Long:  "A command-line interface for all Rhombus API operations.",
}

func init() {
	rootCmd.PersistentFlags().String("profile", "default", "Configuration profile to use")
	rootCmd.PersistentFlags().String("output", "", "Output format: json, table, text")
	rootCmd.PersistentFlags().String("api-key", "", "Override API key")
	rootCmd.PersistentFlags().String("endpoint-url", "", "Override endpoint URL")

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
