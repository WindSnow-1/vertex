package main

import (
	"encoding/json"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

	"vertex/internal/apikey"
	"vertex/internal/gemini"
	"vertex/internal/metrics"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[PANIC] %v\n%s", err, debug.Stack())
				http.Error(w, "internal server error", 500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func withAPIKey(keys *apikey.Manager, col *metrics.Collector, handler func(http.ResponseWriter, *http.Request, *gemini.RequestContext)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, 401, map[string]any{"error": map[string]any{
				"message": "missing or invalid API key",
				"type":    "invalid_request_error",
			}})
			return
		}
		key := strings.TrimSpace(auth[7:])
		if !keys.Validate(key) {
			writeJSON(w, 401, map[string]any{"error": map[string]any{
				"message": "invalid API key",
				"type":    "invalid_request_error",
			}})
			return
		}

		col.IncTotal()
		ctx := &gemini.RequestContext{Collector: col}
		handler(w, r, ctx)
	}
}
