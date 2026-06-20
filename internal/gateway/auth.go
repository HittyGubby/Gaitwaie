package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// receiverContextKey is used to store receiver info in request context.
type receiverContextKey struct{}

// receiverInfo holds the authenticated receiver's name and key.
type receiverInfo struct {
	Name string
	Key  string
}

// authMiddleware returns an HTTP middleware that validates the Authorization header
// against the configured receivers. It skips auth for paths in skipPaths.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	skipPaths := map[string]bool{
		"GET /v1/models": true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for exempted paths
		key := r.Method + " " + r.URL.Path
		if skipPaths[key] {
			next.ServeHTTP(w, r)
			return
		}

		// Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			writeOpenAIError(w, http.StatusUnauthorized, "Unauthorized", "authentication_error", "401")
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Validate against receivers
		for name, key := range s.cfg.Receivers {
			if key == token {
				info := &receiverInfo{Name: name, Key: key}
				ctx := context.WithValue(r.Context(), receiverContextKey{}, info)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		writeOpenAIError(w, http.StatusUnauthorized, "Unauthorized", "authentication_error", "401")
	})
}

// getReceiverFromContext extracts receiver info from the request context.
func getReceiverFromContext(r *http.Request) *receiverInfo {
	if info, ok := r.Context().Value(receiverContextKey{}).(*receiverInfo); ok {
		return info
	}
	return nil
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
	json.NewEncoder(w).Encode(resp)
}
