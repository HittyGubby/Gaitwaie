package models

import "time"

// Config represents the top-level YAML configuration.
type Config struct {
	DatabasePath       string              `yaml:"database_path"`
	ListenAddr         string              `yaml:"listen_addr"`
	Tolerance          int                 `yaml:"tolerance"`
	MaxConcurrentTasks int                 `yaml:"max_concurrent_tasks"`
	StripParams        *[]string           `yaml:"strip_params,omitempty"`
	Providers          map[string]Provider `yaml:"providers"`
	Receivers          map[string]string   `yaml:"receivers"`
}

// Provider represents an upstream AI provider.
type Provider struct {
	BaseURL string   `yaml:"base_url"`
	Keys    []string `yaml:"keys"`
}

// KeyState represents the dynamic state of an upstream API key.
type KeyState struct {
	KeyValue      string
	ProviderAlias string
	FailCount     int
	IsActive      bool
	CoolDownUntil *time.Time
	UpdatedAt     time.Time
}

// RequestLog represents a single proxied request log entry.
// When used for aggregated stats (from QueryStats), RequestCount carries the row count.
type RequestLog struct {
	ID                 int
	Timestamp          time.Time
	StatusCode         int
	PromptTokens       int  // billable prompt tokens (cache miss, falls back to total)
	CompletionTokens   int
	TotalTokens        int
	CachedPromptTokens int  // prompt tokens served from cache (0 if not reported)
	RequestCount       int  // populated only in aggregated queries
	ProviderAlias      string
	RequestedModel     string
	AssignedKey        string
	ReceiverName       string
	IsTestRequest      bool
}

// OpenAIModel represents a model returned by the OpenAI-compatible /v1/models endpoint.
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse is the JSON response for GET /v1/models.
type ModelsResponse struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}

// ChatCompletionRequest is a simplified representation of the incoming request body.
type ChatCompletionRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []any  `json:"messages"`
}

// Usage represents token usage from a stream final chunk.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIError is the standard OpenAI error JSON structure.
type OpenAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}
