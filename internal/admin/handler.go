package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"vertex/internal/apikey"
	"vertex/internal/config"
	"vertex/internal/metrics"
	"vertex/internal/nodes"
	"vertex/internal/proxy"
	"vertex/internal/proxypool"
)

const (
	cookieName = "admin_token"
	sessionTTL = 24 * time.Hour
)

var (
	sessionsMu sync.Mutex
	sessions   = map[string]time.Time{}
)

func issueToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	sessionsMu.Lock()
	sessions[tok] = time.Now().Add(sessionTTL)
	sessionsMu.Unlock()
	return tok
}

func checkToken(tok string) bool {
	if tok == "" {
		return false
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	exp, ok := sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(sessions, tok)
		return false
	}
	return true
}

func dropToken(tok string) {
	sessionsMu.Lock()
	delete(sessions, tok)
	sessionsMu.Unlock()
}

func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func requireAuth(r *http.Request) bool {
	return checkToken(tokenFromRequest(r))
}

func EnsurePassword() {
	cfg := config.Load()
	if strings.TrimSpace(cfg.AdminPassword) != "" {
		return
	}
	b := make([]byte, 9)
	rand.Read(b)
	pw := base64.RawURLEncoding.EncodeToString(b)
	config.WriteSettings(map[string]any{"admin_password": pw})
	log.Printf("============================================================")
	log.Printf("[admin] Generated password: %s", pw)
	log.Printf("[admin] Access: http://<host>:<port>/admin")
	log.Printf("============================================================")
}

func HandlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "admin.html"
	}
	data, err := fs.ReadFile(Assets, "assets/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		w.Header().Set("Content-Type", "image/jpeg")
	}
	w.Write(data)
}

func HandleAPI(keys *apikey.Manager, col *metrics.Collector, cfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/admin")

		if path == "/login" {
			handleLogin(w, r)
			return
		}
		if path == "/check-auth" {
			handleCheckAuth(w, r)
			return
		}

		if strings.HasPrefix(path, "/keys/") {
			if !requireAuth(r) {
				writeJSON(w, 401, map[string]any{"error": "unauthorized"})
				return
			}
			handleDeleteKey(w, r, keys, strings.TrimPrefix(path, "/keys/"))
			return
		}

		if !requireAuth(r) {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}

		switch path {
		case "/logout":
			handleLogout(w, r)
		case "/settings":
			switch r.Method {
			case http.MethodGet:
				handleGetSettings(w)
			case http.MethodPut:
				handlePutSettings(w, r)
			}
		case "/stats":
			writeJSON(w, 200, col.Stats())
		case "/stats/reset":
			col.Reset()
			writeJSON(w, 200, map[string]any{"ok": true})
		case "/history":
			writeJSON(w, 200, map[string]any{"history": col.RecentRequests()})
		case "/keys":
			switch r.Method {
			case http.MethodGet:
				handleGetKeys(w, keys)
			case http.MethodPost:
				handleAddKey(w, r, keys)
			}
		case "/models":
			switch r.Method {
			case http.MethodGet:
				handleGetModels(w)
			case http.MethodPut:
				handlePutModels(w, r)
			}
		case "/nodes":
			switch r.Method {
			case http.MethodGet:
				handleGetNodes(w)
			case http.MethodDelete:
				handleDeleteNode(w, r)
			}
		case "/nodes/test":
			handleTestNode(w, r)
		case "/nodes/test-all":
			handleTestAll(w)
		case "/nodes/deduplicate":
			handleDedupNodes(w)
		case "/nodes/disabled":
			handleDeleteDisabled(w)
		case "/nodes/import":
			handleImportNodes(w, r)
		case "/nodes/batch-enable":
			handleBatchEnable(w, r)
		case "/nodes/batch-disable":
			handleBatchDisable(w, r)
		case "/nodes/batch-delete":
			handleBatchDelete(w, r)
		case "/use-node":
			handleUseNode(w, r)
		case "/subscriptions/fetch":
			handleFetchSub(w, r)
		case "/proxies":
			switch r.Method {
			case http.MethodGet:
				handleGetProxies(w)
			case http.MethodPost:
				handleAddProxy(w, r)
			case http.MethodDelete:
				handleDeleteProxy(w, r)
			}
		case "/proxies/import":
			handleImportProxies(w, r)
		case "/proxies/test":
			handleTestProxy(w, r)
		case "/proxies/test-all":
			handleTestAllProxies(w)
		case "/proxies/batch-enable":
			handleProxyBatchEnable(w, r)
		case "/proxies/batch-disable":
			handleProxyBatchDisable(w, r)
		case "/proxies/batch-delete":
			handleProxyBatchDelete(w, r)
		case "/proxies/deduplicate":
			handleProxyDedup(w)
		case "/proxies/disabled":
			handleDeleteDisabledProxies(w)
		default:
			writeJSON(w, 404, map[string]any{"error": "not found"})
		}
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	expected := strings.TrimSpace(config.Load().AdminPassword)
	if expected == "" {
		writeJSON(w, 500, map[string]any{"error": "admin password not set"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Password)), []byte(expected)) != 1 {
		log.Printf("[admin] login failed from %s", r.RemoteAddr)
		writeJSON(w, 401, map[string]any{"error": "invalid password"})
		return
	}
	tok := issueToken()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL / time.Second),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	dropToken(tokenFromRequest(r))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleCheckAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"authenticated": requireAuth(r)})
}

func handleGetSettings(w http.ResponseWriter) {
	cfg := config.Load()
	writeJSON(w, 200, map[string]any{"settings": map[string]any{
		"max_retries":             cfg.MaxRetries,
		"token_pool_size":         cfg.TokenPoolSize,
		"max_spill_mb":            cfg.MaxSpillMB,
		"max_request_mb":          cfg.MaxRequestMB,
		"max_n":                   cfg.MaxN,
		"anti429_enabled":         cfg.Anti429Enabled,
		"anti429_target":          cfg.Anti429Target,
		"force_no_stream":         cfg.ForceNoStream,
		"anti_tracking":           cfg.AntiTracking,
		"drop_max_tokens":         cfg.DropMaxTokens,
		"proxy_url":               cfg.ProxyURL,
		"active_node_uri":         cfg.ActiveNodeURI,
		"parallel_pool_enabled":   cfg.ParallelPoolEnabled,
		"parallel_pool_size":      cfg.ParallelPoolSize,
		"parallel_pool_max_rounds": cfg.ParallelPoolMaxRounds,
		"parallel_node_top_k":     cfg.ParallelNodeTopK,
		"proxy_pool_enabled":      cfg.ProxyPoolEnabled,
	}})
}

var allowedSettings = map[string]bool{
	"max_retries": true, "token_pool_size": true, "max_spill_mb": true,
	"max_request_mb": true, "max_n": true, "anti429_enabled": true,
	"anti429_target": true, "force_no_stream": true, "anti_tracking": true,
	"drop_max_tokens": true, "proxy_url": true, "admin_password": true,
	"parallel_pool_enabled": true, "parallel_pool_size": true,
	"parallel_pool_max_rounds": true, "parallel_node_top_k": true,
	"active_node_uri": true, "vertex_api_key": true,
	"proxy_pool_enabled": true,
}

func handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	updates := map[string]any{}
	for k, v := range body.Settings {
		if !allowedSettings[k] {
			continue
		}
		switch k {
		case "max_retries", "token_pool_size", "max_spill_mb", "max_request_mb", "max_n":
			if f, ok := v.(float64); ok {
				updates[k] = int(f)
				continue
			}
		case "admin_password":
			if pw, ok := v.(string); !ok || strings.TrimSpace(pw) == "" {
				continue
			} else {
				updates[k] = strings.TrimSpace(pw)
				continue
			}
		}
		updates[k] = v
	}
	if err := config.WriteSettings(updates); err != nil {
		writeJSON(w, 500, map[string]any{"error": "failed to write config"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleGetKeys(w http.ResponseWriter, keys *apikey.Manager) {
	entries := keys.List()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"name":       e.Name,
			"key":        e.Key,
			"key_masked": apikey.MaskKey(e.Key),
		})
	}
	writeJSON(w, 200, map[string]any{"keys": out})
}

func handleAddKey(w http.ResponseWriter, r *http.Request, keys *apikey.Manager) {
	var body struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	key := strings.TrimSpace(body.Key)
	if name == "" {
		writeJSON(w, 400, map[string]any{"error": "name is required"})
		return
	}
	if key != "" && !apikey.HasPrefix(key) {
		writeJSON(w, 400, map[string]any{"error": "key must start with sk-"})
		return
	}
	if key == "" {
		key = apikey.GenerateKey()
	}
	if err := keys.Add(name, key, body.Description); err != nil {
		writeJSON(w, 500, map[string]any{"error": "failed to save key"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": key})
}

func handleDeleteKey(w http.ResponseWriter, r *http.Request, keys *apikey.Manager, rawName string) {
	if r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	name := rawName
	if dec, err := url.PathUnescape(rawName); err == nil {
		name = dec
	}
	ok, err := keys.Delete(name)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "failed to delete key"})
		return
	}
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "key not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleGetModels(w http.ResponseWriter) {
	mc := config.LoadModels()
	writeJSON(w, 200, map[string]any{"models": mc.Models, "alias_map": mc.AliasMap})
}

func handlePutModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Models   []string          `json:"models"`
		AliasMap map[string]string `json:"alias_map"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	cleaned := make([]string, 0)
	for _, m := range body.Models {
		if m = strings.TrimSpace(m); m != "" {
			cleaned = append(cleaned, m)
		}
	}
	if len(cleaned) == 0 {
		writeJSON(w, 400, map[string]any{"error": "models list cannot be empty"})
		return
	}
	alias := map[string]string{}
	for k, v := range body.AliasMap {
		if k, v = strings.TrimSpace(k), strings.TrimSpace(v); k != "" && v != "" {
			alias[k] = v
		}
	}
	if err := config.WriteModels(cleaned, alias); err != nil {
		writeJSON(w, 500, map[string]any{"error": "failed to write models"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleGetNodes(w http.ResponseWriter) {
	list := nodes.LoadNodes()
	var enabled, disabled int
	for _, n := range list {
		if n.Disabled {
			disabled++
		} else {
			enabled++
		}
	}
	writeJSON(w, 200, map[string]any{
		"nodes":          list,
		"health":         nodes.LoadHealth(),
		"total":          len(list),
		"enabled_count":  enabled,
		"disabled_count": disabled,
	})
}

func handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	nodes.DeleteNode(body.RawURI)
	proxy.RemoveProxy(body.RawURI)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleTestNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	start := time.Now()
	transport, err := proxy.TransportForNode(body.RawURI)
	var testErr error
	if err != nil {
		testErr = err
	} else {
		client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://generativelanguage.googleapis.com", nil)
		resp, err := client.Do(req)
		if err != nil {
			testErr = err
		} else {
			resp.Body.Close()
		}
	}
	elapsed := float64(time.Since(start).Milliseconds())
	ok := testErr == nil
	errStr := ""
	if testErr != nil {
		errStr = testErr.Error()
	}
	nodes.RecordTest(body.RawURI, ok, elapsed, errStr)
	writeJSON(w, 200, map[string]any{"ok": ok, "elapsed_ms": elapsed, "error": errStr})
}

func handleTestAll(w http.ResponseWriter) {
	go func() {
		list := nodes.LoadNodes()
		sem := make(chan struct{}, 10)
		var wg sync.WaitGroup
		for _, n := range list {
			wg.Add(1)
			go func(node nodes.Node) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				start := time.Now()
				transport, err := proxy.TransportForNode(node.RawURI)
				var testErr error
				if err != nil {
					testErr = err
				} else {
					client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					req, _ := http.NewRequestWithContext(ctx, "GET", "https://generativelanguage.googleapis.com", nil)
					resp, err := client.Do(req)
					if err != nil {
						testErr = err
					} else {
						resp.Body.Close()
					}
				}
				elapsed := float64(time.Since(start).Milliseconds())
				ok := testErr == nil
				errStr := ""
				if testErr != nil {
					errStr = testErr.Error()
				}
				nodes.RecordTest(node.RawURI, ok, elapsed, errStr)
				if ok {
					log.Printf("[nodes] OK %s: %.0fms", node.Name, elapsed)
				} else {
					log.Printf("[nodes] FAIL %s: %s", node.Name, errStr)
				}
			}(n)
		}
		wg.Wait()
		log.Printf("[admin] test-all complete")
	}()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleDedupNodes(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"ok": true, "removed_count": nodes.DedupNodes()})
}

func handleDeleteDisabled(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"ok": true, "deleted_count": nodes.DeleteDisabled()})
}

func handleImportNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	parsed := nodes.ParseSubscriptionText(body.Text)
	if body.Replace {
		all := nodes.LoadNodes()
		for _, n := range all {
			nodes.DeleteNode(n.RawURI)
		}
	}
	count := nodes.MergeNodes(parsed)
	writeJSON(w, 200, map[string]any{"ok": true, "count": count})
}

func handleBatchEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	nodes.BatchUpdateDisabled(body.URIs, false)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleBatchDisable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	nodes.BatchUpdateDisabled(body.URIs, true)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	nodes.BatchDelete(body.URIs)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleUseNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	_ = config.WriteSettings(map[string]any{"active_node_uri": body.RawURI, "parallel_pool_enabled": false})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleFetchSub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	if body.URL == "" {
		writeJSON(w, 400, map[string]any{"error": "url is required"})
		return
	}
	newNodes, err := nodes.FetchSubscription(body.URL)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	count := nodes.MergeNodes(newNodes)
	writeJSON(w, 200, map[string]any{"ok": true, "count": count})
}

// --- Proxy Pool handlers ---

func handleGetProxies(w http.ResponseWriter) {
	list := proxypool.LoadProxies()
	total, enabled, disabled := proxypool.Count()
	writeJSON(w, 200, map[string]any{
		"proxies":        list,
		"health":         proxypool.Health(),
		"total":          total,
		"enabled_count":  enabled,
		"disabled_count": disabled,
	})
}

func handleAddProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	url := strings.TrimSpace(body.URL)
	if url == "" {
		writeJSON(w, 400, map[string]any{"error": "url is required"})
		return
	}
	proxypool.Add(url, strings.TrimSpace(body.Name))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	proxypool.Delete(body.URL)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleImportProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	count := proxypool.Import(body.Text)
	writeJSON(w, 200, map[string]any{"ok": true, "count": count})
}

func handleTestProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	ms, err := proxypool.TestProxy(body.URL)
	ok := err == nil
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	proxypool.RecordResult(body.URL, ok, ms, errStr)
	writeJSON(w, 200, map[string]any{"ok": ok, "elapsed_ms": ms, "error": errStr})
}

func handleTestAllProxies(w http.ResponseWriter) {
	go proxypool.TestAll()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleProxyBatchEnable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs []string `json:"urls"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	proxypool.BatchSetDisabled(body.URLs, false)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleProxyBatchDisable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs []string `json:"urls"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	proxypool.BatchSetDisabled(body.URLs, true)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleProxyBatchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs []string `json:"urls"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	proxypool.BatchDelete(body.URLs)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleProxyDedup(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"ok": true, "removed_count": proxypool.Dedup()})
}

func handleDeleteDisabledProxies(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"ok": true, "deleted_count": proxypool.DeleteDisabled()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
