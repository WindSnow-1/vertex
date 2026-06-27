package proxypool

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

type Proxy struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

type HealthInfo struct {
	LastTestMs    float64 `json:"last_test_ms"`
	LastTestError string  `json:"last_test_error"`
	LastTestTime  int64   `json:"last_test_time"`
	SuccessCount  int     `json:"success_count"`
	FailCount     int     `json:"fail_count"`
	CooldownUntil int64   `json:"cooldown_until"`
}

type proxiesFile struct {
	Proxies []Proxy `json:"proxies"`
}

var (
	mu      sync.RWMutex
	proxies []Proxy
	health  = map[string]*HealthInfo{}
	counter uint64
	loaded  bool
)

func proxiesPath() string {
	if p := os.Getenv("VERTEX_PROXIES"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "proxies.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join("config", "proxies.json")
}

func load() {
	if loaded {
		return
	}
	loaded = true
	data, err := os.ReadFile(proxiesPath())
	if err != nil {
		return
	}
	var pf proxiesFile
	if json.Unmarshal(data, &pf) == nil {
		proxies = pf.Proxies
	}
}

func save() error {
	pf := proxiesFile{Proxies: proxies}
	if dir := filepath.Dir(proxiesPath()); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, _ := json.MarshalIndent(pf, "", "  ")
	tmp := proxiesPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, proxiesPath())
}

func LoadProxies() []Proxy {
	mu.Lock()
	load()
	out := make([]Proxy, len(proxies))
	copy(out, proxies)
	mu.Unlock()
	return out
}

func SaveProxies(list []Proxy) error {
	mu.Lock()
	defer mu.Unlock()
	proxies = list
	loaded = true
	return save()
}

func Next() string {
	mu.RLock()
	defer mu.RUnlock()
	load()

	now := time.Now().Unix()
	enabled := make([]Proxy, 0, len(proxies))
	for _, p := range proxies {
		if p.Disabled {
			continue
		}
		if h, ok := health[p.URL]; ok && h.CooldownUntil > now {
			continue
		}
		enabled = append(enabled, p)
	}
	if len(enabled) == 0 {
		return ""
	}
	idx := atomic.AddUint64(&counter, 1) - 1
	return enabled[idx%uint64(len(enabled))].URL
}

func RecordResult(proxyURL string, ok bool, latencyMs float64, errMsg string) {
	mu.Lock()
	defer mu.Unlock()

	h, exists := health[proxyURL]
	if !exists {
		h = &HealthInfo{}
		health[proxyURL] = h
	}
	h.LastTestMs = latencyMs
	h.LastTestError = errMsg
	h.LastTestTime = time.Now().Unix()
	if ok {
		h.SuccessCount++
		h.CooldownUntil = 0
	} else {
		h.FailCount++
		h.CooldownUntil = time.Now().Unix() + 60
	}
}

func Health() map[string]*HealthInfo {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]*HealthInfo, len(health))
	for k, v := range health {
		cp := *v
		out[k] = &cp
	}
	return out
}

func Add(proxyURL, name string) {
	mu.Lock()
	defer mu.Unlock()
	load()
	for _, p := range proxies {
		if p.URL == proxyURL {
			mu.Unlock()
			mu.Lock()
			return
		}
	}
	if name == "" {
		name = proxyURL
	}
	proxies = append(proxies, Proxy{URL: proxyURL, Name: name})
	_ = save()
}

func Delete(proxyURL string) {
	mu.Lock()
	defer mu.Unlock()
	load()
	for i, p := range proxies {
		if p.URL == proxyURL {
			proxies = append(proxies[:i], proxies[i+1:]...)
			delete(health, proxyURL)
			_ = save()
			return
		}
	}
}

func BatchDelete(urls []string) {
	mu.Lock()
	defer mu.Unlock()
	load()
	set := map[string]bool{}
	for _, u := range urls {
		set[u] = true
	}
	filtered := make([]Proxy, 0, len(proxies))
	for _, p := range proxies {
		if !set[p.URL] {
			filtered = append(filtered, p)
		} else {
			delete(health, p.URL)
		}
	}
	proxies = filtered
	_ = save()
}

func BatchSetDisabled(urls []string, disabled bool) {
	mu.Lock()
	defer mu.Unlock()
	load()
	set := map[string]bool{}
	for _, u := range urls {
		set[u] = true
	}
	for i := range proxies {
		if set[proxies[i].URL] {
			proxies[i].Disabled = disabled
		}
	}
	_ = save()
}

func DeleteDisabled() int {
	mu.Lock()
	defer mu.Unlock()
	load()
	count := 0
	filtered := make([]Proxy, 0, len(proxies))
	for _, p := range proxies {
		if p.Disabled {
			count++
			delete(health, p.URL)
		} else {
			filtered = append(filtered, p)
		}
	}
	proxies = filtered
	_ = save()
	return count
}

func Dedup() int {
	mu.Lock()
	defer mu.Unlock()
	load()
	seen := map[string]bool{}
	filtered := make([]Proxy, 0, len(proxies))
	removed := 0
	for _, p := range proxies {
		if seen[p.URL] {
			removed++
			continue
		}
		seen[p.URL] = true
		filtered = append(filtered, p)
	}
	proxies = filtered
	_ = save()
	return removed
}

func Import(text string) int {
	mu.Lock()
	defer mu.Unlock()
	load()

	existing := map[string]bool{}
	for _, p := range proxies {
		existing[p.URL] = true
	}

	lines := strings.Split(text, "\n")
	count := 0
	pendingIP := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "port:") || strings.HasPrefix(lower, "port：") {
			portStr := strings.TrimSpace(line[strings.Index(lower, ":")+1:])
			if pendingIP != "" && portStr != "" {
				full := "http://" + pendingIP + ":" + portStr
				if !existing[full] {
					existing[full] = true
					proxies = append(proxies, Proxy{URL: full, Name: pendingIP + ":" + portStr})
					count++
				}
				pendingIP = ""
			}
			continue
		}

		// Check if line looks like a bare IP (no port, no scheme)
		trimmed := strings.TrimSpace(line)
		if isBareIP(trimmed) {
			pendingIP = trimmed
			continue
		}

		// Flush any pending IP without port
		if pendingIP != "" {
			full := "http://" + pendingIP
			if !existing[full] {
				existing[full] = true
				proxies = append(proxies, Proxy{URL: full, Name: pendingIP})
				count++
			}
			pendingIP = ""
		}

		// Normal line: ip:port or full URL
		if !strings.Contains(trimmed, "://") {
			trimmed = "http://" + trimmed
		}
		if !existing[trimmed] {
			existing[trimmed] = true
			proxies = append(proxies, Proxy{URL: trimmed, Name: trimmed})
			count++
		}
	}

	// Flush last pending IP
	if pendingIP != "" {
		full := "http://" + pendingIP
		if !existing[full] {
			existing[full] = true
			proxies = append(proxies, Proxy{URL: full, Name: pendingIP})
			count++
		}
	}

	_ = save()
	return count
}

func isBareIP(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, ":") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func TestProxy(proxyURL string) (float64, error) {
	start := time.Now()
	transport, err := buildTransport(proxyURL)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://httpbin.org/ip", nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return float64(time.Since(start).Milliseconds()), nil
}

func TestAll() {
	list := LoadProxies()
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, p := range list {
		wg.Add(1)
		go func(px Proxy) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ms, err := TestProxy(px.URL)
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			RecordResult(px.URL, err == nil, ms, errStr)
			if err != nil {
				log.Printf("[proxypool] FAIL %s: %s", px.URL, errStr)
			} else {
				log.Printf("[proxypool] OK %s: %.0fms", px.URL, ms)
			}
		}(p)
	}
	wg.Wait()
	log.Printf("[proxypool] test-all complete")
}

func buildTransport(proxyURL string) (http.RoundTripper, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		return &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
			TLSHandshakeTimeout: 10 * time.Second,
		}, nil
	default:
		return &http.Transport{
			Proxy:               http.ProxyURL(u),
			TLSHandshakeTimeout: 10 * time.Second,
		}, nil
	}
}

func Count() (total, enabled, disabled int) {
	mu.RLock()
	defer mu.RUnlock()
	load()
	for _, p := range proxies {
		total++
		if p.Disabled {
			disabled++
		} else {
			enabled++
		}
	}
	return
}
