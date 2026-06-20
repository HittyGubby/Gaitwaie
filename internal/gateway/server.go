package gateway

import (
	"log"
	"net/http"
	"sync/atomic"

	"github.com/HittyGubby/gaitwaie/internal/database"
	"github.com/HittyGubby/gaitwaie/internal/models"
)

// providerConfig is an alias for the provider configuration used within the gateway package.
type providerConfig = models.Provider

// Server holds all shared state for the HTTP gateway.
type Server struct {
	cfg        *models.Config
	db         *database.DB
	httpClient *http.Client
	modelCache *ModelCache
	rrCounters map[string]*atomic.Uint64 // per-provider round-robin counters
	httpServer *http.Server
}

// NewServer creates and initializes a new gateway server.
// It does NOT start the HTTP listener — call Serve() to do that.
func NewServer(cfg *models.Config, db *database.DB) *Server {
	s := &Server{
		cfg:        cfg,
		db:         db,
		httpClient: &http.Client{},
		modelCache: newModelCache(),
		rrCounters: make(map[string]*atomic.Uint64),
	}

	// Initialize round-robin counters for each provider
	for alias := range cfg.Providers {
		s.rrCounters[alias] = new(atomic.Uint64)
	}

	return s
}

// RefreshModelsAsync starts an async refresh of the model cache.
// It's designed to be called during server startup.
func (s *Server) RefreshModelsAsync() {
	go func() {
		if err := s.RefreshModels(); err != nil {
			log.Printf("[gateway] initial model refresh failed: %v", err)
		}
	}()
}

// Serve starts the HTTP listener and blocks until shutdown.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()

	// Register routes
	mux.HandleFunc("GET /v1/models", s.getModelsHandler)
	mux.HandleFunc("POST /v1/chat/completions", s.postChatCompletionsHandler)

	// Wrap with auth middleware
	handler := s.authMiddleware(mux)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	log.Printf("[gateway] listening on %s", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown() error {
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// pickKey selects an active key from the given list using round-robin.
// indices is a pre-filtered list of indexes into keys that are active.
func (s *Server) pickKey(alias string, keys []models.KeyState) *models.KeyState {
	if len(keys) == 0 {
		return nil
	}
	counter := s.rrCounters[alias]
	idx := int(counter.Add(1) % uint64(len(keys)))
	return &keys[idx]
}
