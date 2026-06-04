package cmd

import (
	"fmt"
	"os"

	"github.com/RhombusSystems/rhombus-cli/internal/config"
	"github.com/spf13/cobra"
)

// pluginMarketplace is the Claude Code marketplace that hosts the Rhombus plugins.
// NOTE: Claude Code only acts on hints targeting the official Anthropic marketplace
// (claude-plugins-official); until the Rhombus plugins are listed there this hint is
// emitted but silently dropped. Change this one constant when that lands.
const pluginMarketplace = "rhombus-plugins"

const (
	pluginUser    = "rhombus-user"
	pluginPartner = "rhombus-partner"
)

// maybeEmitPluginHint writes a Claude Code plugin-install hint to stderr when running
// inside Claude Code (CLAUDECODE set). The marker is written on its own line so Claude
// Code can strip it before the output reaches the model; it is never shown to users
// running the CLI directly. See https://code.claude.com/docs/en/plugin-hints.md
func maybeEmitPluginHint(cmd *cobra.Command) {
	if os.Getenv("CLAUDECODE") == "" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"<claude-code-hint v=\"1\" type=\"plugin\" value=\"%s@%s\" />\n",
		recommendedPlugin(cmd), pluginMarketplace)
}

// recommendedPlugin picks the plugin to recommend from CLI context: the partner plugin
// when operating in a partner / multi-org context, otherwise the user plugin.
func recommendedPlugin(cmd *cobra.Command) string {
	if org, _ := cmd.Root().PersistentFlags().GetString("partner-org"); org != "" {
		return pluginPartner
	}
	if cfg := config.LoadFromCmd(cmd); cfg.IsPartner {
		return pluginPartner
	}
	return pluginUser
}
