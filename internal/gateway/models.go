package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ModelCache holds the aggregated model list in memory.
type ModelCache struct {
	mu      sync.RWMutex
	models  map[string][]string       // provider_alias -> []prefixed_model_id
	rawData []map[string]interface{}   // cached JSON response data
}

// newModelCache creates an empty ModelCache.
func newModelCache() *ModelCache {
	return &ModelCache{
		models: make(map[string][]string),
	}
}

// GetModels returns a copy of the cached model IDs per provider.
func (mc *ModelCache) GetModels() map[string][]string {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	result := make(map[string][]string, len(mc.models))
	for k, v := range mc.models {
		models := make([]string, len(v))
		copy(models, v)
		result[k] = models
	}
	return result
}

// RefreshModels queries each provider's /v1/models and builds the cache.
func (s *Server) RefreshModels() error {
	cache := newModelCache()

	var mu sync.Mutex
	var wg sync.WaitGroup

	for alias, provider := range s.cfg.Providers {
		wg.Add(1)
		go func(alias string, provider providerConfig) {
			defer wg.Done()

			models, err := FetchProviderModels(s.httpClient, provider.BaseURL, provider.Keys, alias)
			if err != nil {
				log.Printf("[models] failed to fetch models for provider %q: %v", alias, err)
				return
			}

			mu.Lock()
			cache.models[alias] = models
			mu.Unlock()

			// Persist to SQLite for CLI access (manage command)
			if err := s.db.SaveModelCache(alias, models); err != nil {
				log.Printf("[models] failed to persist models for %q: %v", alias, err)
			}
		}(alias, provider)
	}

	wg.Wait()

	// Build the flat JSON data for the /v1/models response
	cache.buildResponseData()

	s.modelCache = cache

	log.Printf("[models] cached %d provider(s)", len(cache.models))
	return nil
}

func FetchProviderModels(client *http.Client, baseURL string, keys []string, alias string) ([]string, error) {
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
			prefixed := alias + "/" + m.ID
			models = append(models, prefixed)
		}
		return models, nil
	}

	return nil, fmt.Errorf("all keys failed: %w", lastErr)
}

func (mc *ModelCache) buildResponseData() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	var data []map[string]interface{}
	for _, models := range mc.models {
		for _, m := range models {
			data = append(data, map[string]interface{}{
				"id":       m,
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "system",
			})
		}
	}
	mc.rawData = data
}

// getModelsHandler handles GET /v1/models requests.
func (s *Server) getModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.modelCache.mu.RLock()
	data := s.modelCache.rawData
	s.modelCache.mu.RUnlock()

	resp := map[string]interface{}{
		"object": "list",
		"data":   data,
	}
	json.NewEncoder(w).Encode(resp)
}
