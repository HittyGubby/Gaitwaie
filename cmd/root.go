package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var configPath string

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Gaitwaie - Multi-Tenant AI Router Gateway",
	Long: `Gaitwaie is a minimal, high-performance multi-tenant AI router gateway
that routes OpenAI-compatible API requests to upstream providers.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "/etc/gaitwaie/config.yaml", "Path to config YAML file")
}
