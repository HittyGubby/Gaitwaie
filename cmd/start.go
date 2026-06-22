package cmd

import (
	"fmt"
	"log"

	"github.com/HittyGubby/gaitwaie/internal/config"
	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/HittyGubby/gaitwaie/internal/gateway"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(startCmd)
}

func init() {
	startCmd.Flags().StringVar(&startListenAddr, "listen", "", "Listen address (overrides config's listen_addr)")
}

var startListenAddr string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the gateway HTTP server",
	Long: `Start the gateway HTTP server that listens for OpenAI-compatible API requests
and routes them to upstream providers.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		db, err := database.Open(cfg.DatabasePath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()

		// Sync keys with YAML config (inserts new keys, deactivates removed keys)
		for alias, provider := range cfg.Providers {
			if err := db.SyncKeysExclusive(alias, provider.Keys); err != nil {
				return fmt.Errorf("sync keys for %q: %w", alias, err)
			}
		}

		srv := gateway.NewServer(cfg, db)

		// Start async model refresh
		srv.RefreshModelsAsync()

		// Determine listen address: CLI flag > config > default
		addr := cfg.ListenAddr
		if startListenAddr != "" {
			addr = startListenAddr
		}
		log.Printf("[main] gateway starting on %s", addr)
		return srv.Serve(addr)
	},
}
