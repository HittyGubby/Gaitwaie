package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/models"
)

// postChatCompletionsHandler handles POST /v1/chat/completions with transparent retry.
func (s *Server) postChatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	// Read the original body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Cannot read request body", "invalid_request_error", "400")
		return
	}
	r.Body.Close()

	// Parse the request
	var chatReq struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON", "invalid_request_error", "400")
		return
	}

	if chatReq.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "Model is required", "invalid_request_error", "400")
		return
	}

	// Extract provider alias from model name (format: "provider_alias/model_name")
	parts := strings.SplitN(chatReq.Model, "/", 2)
	alias := parts[0]

	// Verify provider exists
	provider, ok := s.cfg.Providers[alias]
	if !ok {
		writeOpenAIError(w, http.StatusNotFound,
			fmt.Sprintf("Unknown provider alias %q in model %q", alias, chatReq.Model),
			"invalid_request_error", "404")
		return
	}

	receiver := getReceiverFromContext(r)

	// Inject stream_options into the request body
	modifiedBody, err := injectStreamOptions(bodyBytes, chatReq.Stream)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "Failed to process request", "server_error", "500")
		return
	}

	// Strip configured parameters if enabled (prevents upstream 400 errors from oversized/zero limits)
	if s.cfg.StripParams != nil && len(*s.cfg.StripParams) > 0 {
		modifiedBody = stripParams(modifiedBody, *s.cfg.StripParams)
	}

	// Build upstream URL
	upstreamModel := chatReq.Model
	if len(parts) > 1 {
		upstreamModel = parts[1]
	} else {
		upstreamModel = chatReq.Model
	}

	// Replace the model field with the upstream model name
	modifiedBody = replaceModelField(modifiedBody, upstreamModel)

	upstreamURL := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"

	// Phase 1 & 2 transparent retry
	upstreamResp, usedKey, err := s.sendWithRetry(r.Context(), upstreamURL, modifiedBody, alias, chatReq.Model, receiver)
	if err != nil {
		log.Printf("[proxy] all retry attempts exhausted for %q: %v", alias, err)
		writeOpenAIError(w, http.StatusBadGateway,
			fmt.Sprintf("All upstream keys failed for provider %q", alias),
			"upstream_error", "502")
		return
	}
	defer upstreamResp.Body.Close()

	// Handle non-streaming response
	if !chatReq.Stream {
		s.handleNonStreaming(w, upstreamResp, usedKey, chatReq.Model, alias, receiver)
		return
	}

	// Handle streaming response
	s.handleStreaming(w, upstreamResp, usedKey, chatReq.Model, alias, receiver)
}

// sendWithRetry implements two-phase transparent retry.
// Phase 1: try active keys (YAML keys with is_active=1).
// Phase 2 (dead-key resurrection): if all active keys fail, try ALL YAML keys.
// If a Phase 2 key succeeds, it is automatically re-enabled in the database.
func (s *Server) sendWithRetry(ctx context.Context, upstreamURL string, body []byte, alias, model string, receiver *receiverInfo) (*http.Response, string, error) {
	providerCfg := s.cfg.Providers[alias]
	yamlKeys := providerCfg.Keys

	// Phase 1 — active keys only
	activeKeys, err := s.db.GetActiveKeysInList(alias, yamlKeys)
	if err != nil {
		return nil, "", fmt.Errorf("db error: %w", err)
	}
	for _, key := range activeKeys {
		resp, tryErr := s.tryKey(ctx, key.KeyValue, upstreamURL, body, alias, model, receiver)
		if tryErr == nil {
			// Success — reset fail count
			if err := s.db.ResetFailCount(key.KeyValue); err != nil {
				log.Printf("[proxy] failed to reset fail count for %q: %v", maskKey(key.KeyValue), err)
			}
			return resp, key.KeyValue, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Phase 2 — dead-key resurrection: try ALL YAML keys (regardless of is_active)
	log.Printf("[proxy] Phase 1 exhausted for %q, entering Phase 2 dead-key fallback...", alias)
	allKeys, err := s.db.GetAllKeysInList(alias, yamlKeys)
	if err != nil {
		return nil, "", fmt.Errorf("db error: %w", err)
	}
	for _, key := range allKeys {
		resp, tryErr := s.tryKey(ctx, key.KeyValue, upstreamURL, body, alias, model, receiver)
		if tryErr == nil {
			// Resurrect: re-enable and reset fail count
			log.Printf("[proxy] dead key %q resurrected!", maskKey(key.KeyValue))
			if err := s.db.ReenableKey(key.KeyValue); err != nil {
				log.Printf("[proxy] failed to re-enable key %q: %v", maskKey(key.KeyValue), err)
			}
			if err := s.db.ResetFailCount(key.KeyValue); err != nil {
				log.Printf("[proxy] failed to reset fail count for %q: %v", maskKey(key.KeyValue), err)
			}
			return resp, key.KeyValue, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	return nil, "", fmt.Errorf("all keys failed for provider %q", alias)
}

// tryKey sends a single request to upstream with the given key.
// Returns the response on success (HTTP 200). On failure, records the failure
// in the database and returns an error (without writing to the client).
func (s *Server) tryKey(ctx context.Context, keyValue, upstreamURL string, body []byte, alias, model string, receiver *receiverInfo) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keyValue)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Network error — record failure and return
		s.recordKeyFailure(keyValue, 0, err, alias, model, receiver)
		return resp, err
	}

	if resp.StatusCode != http.StatusOK {
		// Non-200 — record failure and return
		// Read body for logging but discard it (retry will create a new request)
		errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		log.Printf("[proxy] key %q returned %s", maskKey(keyValue), errMsg)
		s.recordKeyFailure(keyValue, resp.StatusCode, nil, alias, model, receiver)
		return resp, fmt.Errorf("%s", errMsg)
	}

	return resp, nil
}

// recordKeyFailure logs the failed request and updates the circuit breaker state.
// It does NOT write any response to the client — that's the caller's responsibility.
func (s *Server) recordKeyFailure(keyValue string, statusCode int, netErr error, alias, model string, receiver *receiverInfo) {
	code := statusCode
	if netErr != nil {
		code = 0
	}

	reqLog := &models.RequestLog{
		Timestamp:          time.Now(),
		StatusCode:         code,
		PromptTokens:       0,
		CompletionTokens:   0,
		TotalTokens:        0,
		CachedPromptTokens: 0,
		ProviderAlias:      alias,
		RequestedModel:     model,
		AssignedKey:        keyValue,
		ReceiverName:       "",
		IsTestRequest:      false,
	}
	if receiver != nil {
		reqLog.ReceiverName = receiver.Name
	}
	if err := s.db.InsertRequestLog(reqLog); err != nil {
		log.Printf("[proxy] failed to log request: %v", err)
	}

	if netErr != nil {
		// Network error: increment fail count (circuit breaker)
		broken, incErr := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance, *s.cfg.DisableOnTolerance)
		if incErr != nil {
			log.Printf("[proxy] failed to increment fail count: %v", incErr)
		}
		if broken {
			log.Printf("[proxy] key %q auto-deactivated (network error)", maskKey(keyValue))
		}
	} else {
		// HTTP error: use state machine
		s.updateKeyState(keyValue, statusCode)
	}
}

// handleNonStreaming processes a non-streaming upstream response.
func (s *Server) handleNonStreaming(w http.ResponseWriter, upstreamResp *http.Response, keyValue, model, alias string, receiver *receiverInfo) {
	// Copy response headers
	for k, v := range upstreamResp.Header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(upstreamResp.StatusCode)

	// Copy response body
	bodyBytes, _ := io.ReadAll(upstreamResp.Body)
	w.Write(bodyBytes)

	// Process status code for circuit breaker
	s.updateKeyState(keyValue, upstreamResp.StatusCode)

	// Try to extract token usage from response
	promptTokens, cachedTokens, completionTokens := extractUsageFromBody(bodyBytes)

	reqLog := &models.RequestLog{
		Timestamp:           time.Now(),
		StatusCode:          upstreamResp.StatusCode,
		PromptTokens:        promptTokens,
		CompletionTokens:    completionTokens,
		TotalTokens:         promptTokens + completionTokens,
		CachedPromptTokens:  cachedTokens,
		ProviderAlias:       alias,
		RequestedModel:      model,
		AssignedKey:         keyValue,
		ReceiverName:        "",
		IsTestRequest:       false,
	}
	if receiver != nil {
		reqLog.ReceiverName = receiver.Name
	}
	if err := s.db.InsertRequestLog(reqLog); err != nil {
		log.Printf("[proxy] failed to log request: %v", err)
	}
}

// handleStreaming processes a streaming SSE response from upstream.
func (s *Server) handleStreaming(w http.ResponseWriter, upstreamResp *http.Response, keyValue, model, alias string, receiver *receiverInfo) {
	// Set response headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(upstreamResp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: not a flushable writer, but try anyway
		flusher = nil
	}

	// Use bufio.Scanner to read upstream SSE response line by line
	scanner := bufio.NewScanner(upstreamResp.Body)
	var lastUsageLine string

	for scanner.Scan() {
		line := scanner.Text()

		// Write line to client and flush
		fmt.Fprintf(w, "%s\n", line)
		if flusher != nil {
			flusher.Flush()
		}

		// Track last line containing "usage"
		if strings.Contains(line, `"usage":`) {
			lastUsageLine = line
		}
	}

	// Process status code for circuit breaker
	s.updateKeyState(keyValue, upstreamResp.StatusCode)

	// Extract token usage from last usage line
	promptTokens, cachedTokens, completionTokens := extractUsageFromSSELine(lastUsageLine)

	reqLog := &models.RequestLog{
		Timestamp:           time.Now(),
		StatusCode:          upstreamResp.StatusCode,
		PromptTokens:        promptTokens,
		CompletionTokens:    completionTokens,
		TotalTokens:         promptTokens + completionTokens,
		CachedPromptTokens:  cachedTokens,
		ProviderAlias:       alias,
		RequestedModel:      model,
		AssignedKey:         keyValue,
		ReceiverName:        "",
		IsTestRequest:       false,
	}
	if receiver != nil {
		reqLog.ReceiverName = receiver.Name
	}
	if err := s.db.InsertRequestLog(reqLog); err != nil {
		log.Printf("[proxy] failed to log request: %v", err)
	}
}

// updateKeyState applies the circuit breaker state machine based on upstream status code.
func (s *Server) updateKeyState(keyValue string, statusCode int) {
	switch {
	case statusCode == http.StatusOK:
		// 200: reset fail count
		if err := s.db.ResetFailCount(keyValue); err != nil {
			log.Printf("[proxy] failed to reset fail count for key %q: %v", maskKey(keyValue), err)
		}

	case statusCode == http.StatusTooManyRequests:
		// 429: do nothing (rate limit, not key failure)
		return

	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		// 401/403: direct deactivation
		if err := s.db.DirectDeactivate(keyValue); err != nil {
			log.Printf("[proxy] failed to deactivate key %q: %v", maskKey(keyValue), err)
		}
		log.Printf("[proxy] key %q deactivated due to status %d", maskKey(keyValue), statusCode)

	case statusCode >= 500:
		// 5xx: increment fail count
		broken, err := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance, *s.cfg.DisableOnTolerance)
		if err != nil {
			log.Printf("[proxy] failed to increment fail count: %v", err)
		}
		if broken {
			log.Printf("[proxy] key %q auto-deactivated (fail count >= %d)", maskKey(keyValue), s.cfg.Tolerance)
		}

	default:
		// Other 4xx: increment fail count
		broken, err := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance, *s.cfg.DisableOnTolerance)
		if err != nil {
			log.Printf("[proxy] failed to increment fail count: %v", err)
		}
		if broken {
			log.Printf("[proxy] key %q auto-deactivated (fail count >= %d)", maskKey(keyValue), s.cfg.Tolerance)
		}
	}
}

// injectStreamOptions adds "stream_options": {"include_usage": true} to the request body.
func injectStreamOptions(body []byte, stream bool) ([]byte, error) {
	if !stream {
		return body, nil
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	data["stream_options"] = map[string]interface{}{
		"include_usage": true,
	}

	return json.Marshal(data)
}

// stripParams removes the given parameter names from the request body JSON.
// This prevents upstream 400 errors when clients send oversized or zero max token limits.
func stripParams(body []byte, params []string) []byte {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}

	for _, key := range params {
		delete(data, key)
	}

	result, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return result
}

// replaceModelField replaces the "model" field in the JSON body.
func replaceModelField(body []byte, newModel string) []byte {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}
	data["model"] = newModel
	result, _ := json.Marshal(data)
	return result
}

// extractUsageFromSSELine parses token usage from a single SSE data line.
// Returns (billablePromptTokens, cachedPromptTokens, completionTokens).
// For providers with prompt caching (like DeepSeek), billablePromptTokens is
// prompt_cache_miss_tokens; for others it falls back to prompt_tokens.
func extractUsageFromSSELine(line string) (int, int, int) {
	if line == "" {
		return 0, 0, 0
	}

	// Strip "data: " prefix if present
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
		// No cache miss field: use prompt_tokens minus cached, or full prompt_tokens
		if cached > 0 && u.PromptTokens > cached {
			billable = u.PromptTokens - cached
		} else {
			billable = u.PromptTokens
		}
	}

	return billable, cached, u.CompletionTokens
}

// extractUsageFromBody parses token usage from a non-streaming JSON response body.
func extractUsageFromBody(body []byte) (int, int, int) {
	if len(body) == 0 {
		return 0, 0, 0
	}
	return extractUsageFromSSELine(string(body))
}

// maskKey returns a masked version of an API key for logging (shows first 8 chars).
func maskKey(key string) string {
	if len(key) <= 8 {
		return key[:min(len(key), 4)] + "****"
	}
	return key[:8] + "****"
}
