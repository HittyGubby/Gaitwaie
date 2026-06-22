package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/config"
	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/HittyGubby/gaitwaie/internal/models"
	"github.com/spf13/cobra"
)

type providerModelChoice struct {
	alias string
	model string // full prefixed model name (e.g. "ds/deepseek-chat")
}

type keyTestResult struct {
	keyValue string
	success  bool
	tokens   int
	err      string
}

func init() {
	rootCmd.AddCommand(purgeCmd)
}

var purgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Test provider keys and deactivate invalid ones",
	Long: `Interactively test each provider's API keys by sending a minimal chat request.
For each provider, you'll be asked to select a model for testing.
After testing, failed keys can be deactivated.`,
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

		client := &http.Client{Timeout: 30 * time.Second}
		reader := bufio.NewReader(os.Stdin)

		// Step 1: For each provider, fetch available models and let user select
		choices := make([]providerModelChoice, 0, len(cfg.Providers))

		for alias, provider := range cfg.Providers {
			fmt.Printf("\n>>> Provider [%s] - fetching available models...\n", alias)

			models, err := fetchModelsForPurge(client, provider.BaseURL, provider.Keys, alias)
			if err != nil {
				fmt.Printf("  Failed to fetch models: %v\n", err)
				fmt.Printf("  Enter model name manually (e.g. %s/MODEL_NAME): ", alias)
				model, _ := reader.ReadString('\n')
				model = strings.TrimSpace(model)
				if model == "" {
					model = alias + "/default"
				}
				choices = append(choices, providerModelChoice{alias: alias, model: model})
				continue
			}

			if len(models) == 0 {
				fmt.Printf("  No models found. Enter model name manually (e.g. %s/MODEL_NAME): ", alias)
				model, _ := reader.ReadString('\n')
				model = strings.TrimSpace(model)
				if model == "" {
					model = alias + "/default"
				}
				choices = append(choices, providerModelChoice{alias: alias, model: model})
				continue
			}

			fmt.Printf("  Select model for testing [%s]:\n", alias)
			for i, m := range models {
				fmt.Printf("  %d. %s\n", i+1, m)
			}
			fmt.Print("  Enter number (default 1): ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)

			selected := 0 // default to first
			if input != "" {
				idx := 0
				if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(models) {
					selected = idx - 1
				}
			}

			choices = append(choices, providerModelChoice{alias: alias, model: models[selected]})
			fmt.Printf("  Selected: %s\n", models[selected])
		}

		// Step 2: Test each provider's keys
		fmt.Println("\n========================================")
		fmt.Println("Starting key tests...")

		for _, choice := range choices {
			fmt.Printf("\n>>> Testing %s with model: %s\n", choice.alias, choice.model)

			// Get all active keys for this provider
			keys, err := db.GetActiveKeys(choice.alias)
			if err != nil {
				fmt.Printf("  Failed to get keys for %q: %v\n", choice.alias, err)
				continue
			}

			if len(keys) == 0 {
				fmt.Printf("  No active keys for %s\n", choice.alias)
				continue
			}

			// Test keys with concurrency limit
			results := testKeys(client, cfg, db, choice, keys)

			// Show results
			var failedKeys []string
			for _, r := range results {
				if r.success {
					fmt.Printf("  %s  ✅  %d tokens\n", maskPurgeKey(r.keyValue), r.tokens)
				} else {
					fmt.Printf("  %s  ❌  %s\n", maskPurgeKey(r.keyValue), r.err)
					failedKeys = append(failedKeys, r.keyValue)
				}
			}

			// Ask to remove failed keys
			if len(failedKeys) > 0 {
				fmt.Printf("  Remove %d failed key(s) for provider [%s]? (y/N): ", len(failedKeys), choice.alias)
				input, _ := reader.ReadString('\n')
				input = strings.TrimSpace(strings.ToLower(input))

				if input == "y" || input == "yes" {
					if err := db.DeactivateKeys(failedKeys); err != nil {
						fmt.Printf("  Failed to deactivate keys: %v\n", err)
					} else {
						fmt.Printf("  ✅ Deactivated %d key(s)\n", len(failedKeys))
					}
				} else {
					fmt.Printf("  Skipped deactivation\n")
				}
			} else {
				fmt.Printf("  All keys passed ✅\n")
			}
		}

		fmt.Println("\n✅ Purge complete")
		return nil
	},
}

// fetchModelsForPurge queries a provider's /v1/models endpoint to get available model names.
func fetchModelsForPurge(client *http.Client, baseURL string, keys []string, alias string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	var lastErr error

	for _, key := range keys {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			resp.Body.Close()
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		var data struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			lastErr = err
			continue
		}

		var models []string
		for _, m := range data.Data {
			models = append(models, alias+"/"+m.ID)
		}
		return models, nil
	}

	return nil, fmt.Errorf("all keys failed: %w", lastErr)
}

// testKeys sends test requests for all keys of a provider with controlled concurrency.
func testKeys(client *http.Client, cfg *models.Config, db *database.DB, choice providerModelChoice, keys []models.KeyState) []keyTestResult {
	results := make([]keyTestResult, len(keys))
	sem := make(chan struct{}, cfg.MaxConcurrentTasks)
	var wg sync.WaitGroup

	// Extract the upstream model name (strip provider prefix)
	parts := strings.SplitN(choice.model, "/", 2)
	upstreamModel := choice.model
	if len(parts) > 1 {
		upstreamModel = parts[1]
	}

	provider := cfg.Providers[choice.alias]
	upstreamURL := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"

	testBody := buildTestRequestBody(upstreamModel)

	for i, key := range keys {
		wg.Add(1)
		sem <- struct{}{} // acquire semaphore

		go func(idx int, kv string) {
			defer wg.Done()
			defer func() { <-sem }() // release semaphore

			success, tokens, errStr := sendTestRequest(client, upstreamURL, kv, testBody)
			results[idx] = keyTestResult{
				keyValue: kv,
				success:  success,
				tokens:   tokens,
				err:      errStr,
			}

			// Record the test request in the database
			reqLog := &models.RequestLog{
				Timestamp:           time.Now(),
				StatusCode:          200,
				PromptTokens:        tokens,
				CompletionTokens:    tokens,
				TotalTokens:         tokens,
				CachedPromptTokens:  0,
				ProviderAlias:       choice.alias,
				RequestedModel:      choice.model,
				AssignedKey:         kv,
				ReceiverName:        "purge",
				IsTestRequest:       true,
			}
			if errStr != "" {
				reqLog.StatusCode = 0
			}
			if err := db.InsertRequestLog(reqLog); err != nil {
				fmt.Fprintf(os.Stderr, "  [warn] failed to log test request: %v\n", err)
			}
		}(i, key.KeyValue)
	}

	wg.Wait()
	return results
}

// buildTestRequestBody creates a minimal chat completion request body for testing.
func buildTestRequestBody(model string) []byte {
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
		"max_tokens": 5,
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	data, _ := json.Marshal(body)
	return data
}

// sendTestRequest sends a test request and returns success status, token count, and error message.
func sendTestRequest(client *http.Client, url, key string, body []byte) (bool, int, string) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return false, 0, fmt.Sprintf("request creation failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, fmt.Sprintf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return false, 0, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Read streaming response and extract token usage
	scanner := bufio.NewScanner(resp.Body)
	var lastUsageLine string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, `"usage":`) {
			lastUsageLine = line
		}
	}

	// Parse token usage
	promptTokens, _, completionTokens := extractUsageForPurge(lastUsageLine)
	totalTokens := promptTokens + completionTokens

	// If scanner error, it might still be partial success
	if err := scanner.Err(); err != nil && totalTokens == 0 {
		return false, 0, fmt.Sprintf("stream error: %v", err)
	}

	return true, totalTokens, ""
}

// extractUsageForPurge parses token usage from an SSE line (same logic as in proxy).
func extractUsageForPurge(line string) (int, int, int) {
	if line == "" {
		return 0, 0, 0
	}

	jsonStr := line
	if strings.HasPrefix(line, "data: ") {
		jsonStr = strings.TrimPrefix(line, "data: ")
	}

	var data struct {
		Usage *struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			PromptTokensDetails   *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return 0, 0, 0
	}
	if data.Usage == nil {
		return 0, 0, 0
	}

	u := data.Usage
	cached := u.PromptCacheHitTokens
	if cached == 0 && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}

	billable := u.PromptCacheMissTokens
	if billable == 0 {
		if cached > 0 && u.PromptTokens > cached {
			billable = u.PromptTokens - cached
		} else {
			billable = u.PromptTokens
		}
	}

	return billable, cached, u.CompletionTokens
}

// maskPurgeKey masks a key for display (shows first 8 chars + last 4 chars).
func maskPurgeKey(key string) string {
	if len(key) <= 12 {
		return key[:min(len(key), 6)] + "****"
	}
	return key[:8] + "..." + key[len(key)-4:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
