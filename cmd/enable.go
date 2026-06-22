package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/HittyGubby/gaitwaie/internal/config"
	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(enableCmd)
}

var enableCmd = &cobra.Command{
	Use:   "enable [key]",
	Short: "Re-enable a previously disabled key",
	Long: `List all disabled keys and let you re-enable them interactively.
If a key is provided as argument, enable it directly without interactive selection.`,
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

		// If key provided as argument, enable it directly
		if len(args) == 1 {
			return enableKey(db, args[0])
		}

		// Interactive mode: list disabled keys
		disabledKeys, err := db.GetAllDisabledKeys()
		if err != nil {
			return fmt.Errorf("get disabled keys: %w", err)
		}

		if len(disabledKeys) == 0 {
			fmt.Println("No disabled keys found.")
			return nil
		}

		fmt.Println("Disabled keys:")
		fmt.Println("────────────────────────────────────────────────────────")
		for i, key := range disabledKeys {
			fmt.Printf("  %d. [%s] %s  (fail_count: %d)\n",
				i+1, key.ProviderAlias, maskKey(key.KeyValue), key.FailCount)
		}
		fmt.Println("────────────────────────────────────────────────────────")

		reader := bufio.NewReader(os.Stdin)
		fmt.Print("\nEnter key number to re-enable (or 'q' to quit): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "q" || input == "" {
			fmt.Println("Cancelled.")
			return nil
		}

		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err != nil || idx < 1 || idx > len(disabledKeys) {
			return fmt.Errorf("invalid selection: %q", input)
		}

		selected := disabledKeys[idx-1]
		return enableKey(db, selected.KeyValue)
	},
}

func enableKey(db *database.DB, keyValue string) error {
	key, err := db.GetKey(keyValue)
	if err != nil {
		return fmt.Errorf("key not found: %s", keyValue)
	}

	if key.IsActive {
		fmt.Printf("Key %s is already active.\n", maskKey(keyValue))
		return nil
	}

	if err := db.ReenableKey(keyValue); err != nil {
		return fmt.Errorf("failed to re-enable key: %w", err)
	}

	fmt.Printf("✅ Key [%s] %s re-enabled (fail_count reset to 0)\n", key.ProviderAlias, maskKey(keyValue))
	return nil
}

// maskKey masks a key for display (shows first 8 chars).
func maskKey(key string) string {
	if len(key) <= 8 {
		return key[:min(len(key), 4)] + "****"
	}
	return key[:8] + "****"
}
