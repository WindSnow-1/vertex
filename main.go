package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"vertex/internal/admin"
	"vertex/internal/apikey"
	"vertex/internal/config"
	"vertex/internal/gemini"
	"vertex/internal/metrics"
	"vertex/internal/nodes"
	"vertex/internal/proxy"
)

func main() {
	cfg := config.Load()

	admin.EnsurePassword()

	keys := apikey.NewManager()
	keys.Load()

	nodes.OnDeleteNode = proxy.RemoveProxy
	proxy.StartGC(5*time.Minute, 30*time.Minute)

	collector := metrics.New()
	client := gemini.NewClient(cfg)

	mux := http.NewServeMux()

	// Public endpoints
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/health", handleHealth(keys))

	// OpenAI-compatible endpoints (require API key)
	mux.HandleFunc("/v1/chat/completions", withAPIKey(keys, collector, client.HandleChatCompletions))
	mux.HandleFunc("/v1/models", withAPIKey(keys, collector, client.HandleModels))
	mux.HandleFunc("/v1/models/", withAPIKey(keys, collector, client.HandleModelInfo))

	// Admin panel
	mux.HandleFunc("/admin", admin.HandlePage)
	mux.HandleFunc("/admin/", admin.HandlePage)
	mux.HandleFunc("/api/admin/", admin.HandleAPI(keys, collector, cfg))

	handler := withCORS(withRecover(mux))

	srv := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(cfg.PortAPI),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("[vertex] shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	addr := "http://localhost:" + strconv.Itoa(cfg.PortAPI) + "/admin"
	log.Printf("[vertex] listening on %s (keys: %d)", srv.Addr, keys.Count())
	go openBrowser(addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[vertex] %v", err)
	}
	log.Println("[vertex] stopped")
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]any{"message": "Vertex AI Proxy", "version": "1.0"})
}

func handleHealth(keys *apikey.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status":          "healthy",
			"timestamp":       time.Now().Unix(),
			"api_keys_loaded": keys.Count(),
		})
	}
}
