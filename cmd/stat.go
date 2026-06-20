package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/config"
	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/spf13/cobra"
)

// durationPattern matches inputs like "30s", "5m", "2h", "7d", "1M", "1y"
var durationPattern = regexp.MustCompile(`^(\d+)\s*(s|m|h|d|M|y)$`)

func init() {
	rootCmd.AddCommand(statCmd)
}

var statCmd = &cobra.Command{
	Use:   "stat [duration]",
	Short: "Show request statistics",
	Long: `Show request statistics grouped by receiver, optionally filtered by a time duration.
Examples:
  gateway stat              - all time stats
  gateway stat 1h           - last 1 hour
  gateway stat 7d           - last 7 days
  gateway stat 30m          - last 30 minutes
  gateway stat 2M           - last 2 months`,
	Args: cobra.RangeArgs(0, 1),
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

		var since *time.Time
		if len(args) == 1 {
			dur, err := parseDuration(args[0])
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", args[0], err)
			}
			t := time.Now().Add(-dur)
			since = &t
		}

		stats, err := db.QueryStats(since)
		if err != nil {
			return fmt.Errorf("query stats: %w", err)
		}

		if len(stats) == 0 {
			fmt.Println("No data")
			return nil
		}

		for name, stat := range stats {
			fmt.Printf("%s:\n", name)
			fmt.Printf("  requests: %d\n", stat.RequestCount)
			fmt.Printf("  prompt: %d (%s)\n", stat.PromptTokens, formatTokens(stat.PromptTokens))
			fmt.Printf("  completion: %d (%s)\n", stat.CompletionTokens, formatTokens(stat.CompletionTokens))
			fmt.Printf("  total: %d (%s)\n", stat.TotalTokens, formatTokens(stat.TotalTokens))
		}

		return nil
	},
}

// parseDuration parses a human-readable duration string into a time.Duration.
// Supported units: s (seconds), m (minutes), h (hours), d (days), M (months), y (years).
func parseDuration(input string) (time.Duration, error) {
	matches := durationPattern.FindStringSubmatch(input)
	if matches == nil {
		return 0, fmt.Errorf("unrecognized format, expected e.g. 30s, 5m, 2h, 7d, 1M, 1y")
	}

	val, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("invalid number: %w", err)
	}

	unit := matches[2]
	switch unit {
	case "s":
		return time.Duration(val) * time.Second, nil
	case "m":
		return time.Duration(val) * time.Minute, nil
	case "h":
		return time.Duration(val) * time.Hour, nil
	case "d":
		return time.Duration(val) * 24 * time.Hour, nil
	case "M":
		return time.Duration(val) * 30 * 24 * time.Hour, nil
	case "y":
		return time.Duration(val) * 365 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
}

// formatTokens formats a token count into a human-readable short form.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}
