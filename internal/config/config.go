package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

type AppConfig struct {
	PortAPI              int    `json:"port_api"`
	MaxRetries           int    `json:"max_retries"`
	AdminPassword        string `json:"admin_password"`
	ProxyURL             string `json:"proxy_url"`
	ForceNoStream        bool   `json:"force_no_stream"`
	AntiTracking         bool   `json:"anti_tracking"`
	DropMaxTokens        bool   `json:"drop_max_tokens"`
	MaxN                 int    `json:"max_n"`
	TokenPoolSize        int    `json:"token_pool_size"`
	MaxSpillMB           int    `json:"max_spill_mb"`
	MaxRequestMB         int    `json:"max_request_mb"`
	Anti429Enabled       bool   `json:"anti429_enabled"`
	Anti429Target        string `json:"anti429_target"`
	VertexAPIKey         string `json:"vertex_api_key"`
	ActiveNodeURI        string `json:"active_node_uri"`
	ParallelPoolEnabled  bool   `json:"parallel_pool_enabled"`
	ParallelPoolSize     int    `json:"parallel_pool_size"`
	ParallelPoolMaxRounds int   `json:"parallel_pool_max_rounds"`
	ParallelNodeTopK     int    `json:"parallel_node_top_k"`
	ProxyPoolEnabled     bool   `json:"proxy_pool_enabled"`
}

func DefaultConfig() AppConfig {
	return AppConfig{
		PortAPI:              2156,
		MaxRetries:           2,
		Anti429Target:        "system",
		AntiTracking:         true,
		MaxN:                 8,
		TokenPoolSize:        8,
		MaxSpillMB:           2048,
		MaxRequestMB:         100,
		ParallelPoolSize:     4,
		ParallelPoolMaxRounds: 0,
		ParallelNodeTopK:     80,
	}
}

var (
	mu     sync.Mutex
	cached *AppConfig
)

func configPath() string {
	if p := os.Getenv("VERTEX_CONFIG"); p != "" {
		return p
	}
	return filepath.Join("config", "config.json")
}

func Path() string { return configPath() }

func Load() *AppConfig {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return cached
	}
	cfg := DefaultConfig()
	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Printf("[config] parse error: %v", err)
		}
	}
	cached = &cfg
	return cached
}

func Reload() *AppConfig {
	mu.Lock()
	cached = nil
	mu.Unlock()
	return Load()
}

func WriteSettings(updates map[string]any) error {
	path := configPath()
	raw := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	for k, v := range updates {
		raw[k] = v
	}
	if err := writeJSONFile(path, raw); err != nil {
		return err
	}
	mu.Lock()
	cached = nil
	mu.Unlock()
	return nil
}

func writeJSONFile(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
