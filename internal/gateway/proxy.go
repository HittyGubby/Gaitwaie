package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/models"
)

// postChatCompletionsHandler handles POST /v1/chat/completions.
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

	// Get an active key via round-robin
	activeKeys, err := s.db.GetActiveKeys(alias)
	if err != nil {
		log.Printf("[proxy] db error for provider %q: %v", alias, err)
		writeOpenAIError(w, http.StatusInternalServerError, "Internal server error", "server_error", "500")
		return
	}
	if len(activeKeys) == 0 {
		writeOpenAIError(w, http.StatusBadGateway,
			fmt.Sprintf("No active keys for provider %q", alias),
			"no_active_keys", "502")
		return
	}

	selected := s.pickKey(alias, activeKeys)
	if selected == nil {
		writeOpenAIError(w, http.StatusBadGateway,
			fmt.Sprintf("No active keys for provider %q", alias),
			"no_active_keys", "502")
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
		// Fallback: use the original model name
		upstreamModel = chatReq.Model
	}

	// Replace the model field with the upstream model name
	modifiedBody = replaceModelField(modifiedBody, upstreamModel)

	upstreamURL := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"

	// Create upstream request
	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "Failed to create upstream request", "server_error", "500")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+selected.KeyValue)

	// Forward the request
	upstreamResp, err := s.httpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("[proxy] upstream request failed for key %q: %v", maskKey(selected.KeyValue), err)
		s.handleUpstreamError(w, selected.KeyValue, err, chatReq.Model, alias, receiver)
		return
	}
	defer upstreamResp.Body.Close()

	// Handle non-streaming response
	if !chatReq.Stream {
		s.handleNonStreaming(w, upstreamResp, selected.KeyValue, chatReq.Model, alias, receiver)
		return
	}

	// Handle streaming response
	s.handleStreaming(w, upstreamResp, selected.KeyValue, chatReq.Model, alias, receiver)
}

// handleUpstreamError processes upstream request failures (network errors, timeouts, etc.).
func (s *Server) handleUpstreamError(w http.ResponseWriter, keyValue string, err error, model, alias string, receiver *receiverInfo) {
	// Record the failed request
	reqLog := &models.RequestLog{
		Timestamp:          time.Now(),
		StatusCode:         0, // no HTTP response
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

	// Increment fail count (network error = circuit breaker trigger)
	broken, err := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance)
	if err != nil {
		log.Printf("[proxy] failed to increment fail count: %v", err)
	}
	if broken {
		log.Printf("[proxy] key %q auto-deactivated due to network error", maskKey(keyValue))
	}

	writeOpenAIError(w, http.StatusBadGateway, "Upstream request failed: "+err.Error(), "upstream_error", "502")
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
		broken, err := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance)
		if err != nil {
			log.Printf("[proxy] failed to increment fail count: %v", err)
		}
		if broken {
			log.Printf("[proxy] key %q auto-deactivated (fail count >= %d)", maskKey(keyValue), s.cfg.Tolerance)
		}

	default:
		// Other 4xx: increment fail count
		broken, err := s.db.IncrementFailCount(keyValue, s.cfg.Tolerance)
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
