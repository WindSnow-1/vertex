package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var fakePrefixes = []string{"假流式-", "fake-"}

func FakePrefixes() []string { return fakePrefixes }

func StripFakePrefix(model string) (string, bool) {
	for _, p := range fakePrefixes {
		if strings.HasPrefix(model, p) {
			return model[len(p):], true
		}
	}
	return model, false
}

var defaultModels = []string{
	"gemini-3.5-flash",
	"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.5-flash-lite",
	"gemini-2.5-flash-image", "gemini-3-flash-preview",
	"gemini-3.1-flash-lite", "gemini-3.1-pro-preview",
	"gemini-3.1-flash-image", "gemini-3-pro-image",
	"gemini-3.1-flash-tts-preview",
}

type ModelsConfig struct {
	Models   []string          `json:"models"`
	AliasMap map[string]string `json:"alias_map"`
}

var (
	modelsMu        sync.Mutex
	modelsCache     *ModelsConfig
	modelsCacheTime time.Time
)

func modelsPath() string {
	if p := os.Getenv("VERTEX_MODELS"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "models.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join("config", "models.json")
}

func loadModelsFile() *ModelsConfig {
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if modelsCache != nil && time.Since(modelsCacheTime) < 60*time.Second {
		return modelsCache
	}

	mc := &ModelsConfig{Models: defaultModels, AliasMap: map[string]string{}}
	if data, err := os.ReadFile(modelsPath()); err == nil {
		var parsed ModelsConfig
		if err := json.Unmarshal(data, &parsed); err != nil {
			log.Printf("[config] parse models.json: %v", err)
		} else if len(parsed.Models) > 0 {
			mc.Models = parsed.Models
			if parsed.AliasMap != nil {
				mc.AliasMap = parsed.AliasMap
			}
		}
	}
	modelsCache = mc
	modelsCacheTime = time.Now()
	return mc
}

func LoadModels() *ModelsConfig {
	mc := loadModelsFile()
	out := &ModelsConfig{
		Models:   make([]string, len(mc.Models)),
		AliasMap: make(map[string]string, len(mc.AliasMap)),
	}
	copy(out.Models, mc.Models)
	for k, v := range mc.AliasMap {
		out.AliasMap[k] = v
	}
	return out
}

func ModelsWithFakeVariants() []string {
	mc := loadModelsFile()
	result := make([]string, 0, len(mc.Models)*3)
	for _, m := range mc.Models {
		result = append(result, m, fakePrefixes[0]+m, fakePrefixes[1]+m)
	}
	return result
}

func ResolveModel(name string) string {
	mc := loadModelsFile()
	if real, ok := mc.AliasMap[name]; ok {
		return real
	}
	return name
}

func WriteModels(models []string, aliasMap map[string]string) error {
	if aliasMap == nil {
		aliasMap = map[string]string{}
	}
	if err := writeJSONFile(modelsPath(), ModelsConfig{Models: models, AliasMap: aliasMap}); err != nil {
		return err
	}
	modelsMu.Lock()
	modelsCache = nil
	modelsCacheTime = time.Time{}
	modelsMu.Unlock()
	return nil
}
