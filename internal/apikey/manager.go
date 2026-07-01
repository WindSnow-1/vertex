package apikey

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"vertex/internal/config"
)

type Entry struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
}

type Manager struct {
	mu      sync.RWMutex
	entries []Entry
	lookup  map[string]bool
}

func NewManager() *Manager {
	return &Manager{lookup: map[string]bool{}}
}

func keysPath() string {
	return filepath.Join(config.ExeDir(), "config", "keys.json")
}

func (m *Manager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(keysPath())
	if err != nil {
		return
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[apikey] parse error: %v", err)
		return
	}
	m.entries = entries
	m.lookup = map[string]bool{}
	for _, e := range entries {
		m.lookup[e.Key] = true
	}
}

func (m *Manager) save() error {
	dir := filepath.Dir(keysPath())
	_ = os.MkdirAll(dir, 0o755)
	data, _ := json.MarshalIndent(m.entries, "", "  ")
	return os.WriteFile(keysPath(), data, 0o644)
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

func (m *Manager) Validate(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lookup[key]
}

func (m *Manager) List() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out
}

func (m *Manager) Add(name, key, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == "" {
		key = GenerateKey()
	}
	m.entries = append(m.entries, Entry{Name: name, Key: key, Description: description})
	m.lookup[key] = true
	return m.save()
}

func (m *Manager) Delete(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, e := range m.entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	delete(m.lookup, m.entries[idx].Key)
	m.entries = append(m.entries[:idx], m.entries[idx+1:]...)
	return true, m.save()
}

func GenerateKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

func MaskKey(key string) string {
	if len(key) <= 4 {
		return "sk-····"
	}
	return "sk-····" + key[len(key)-4:]
}

func HasPrefix(key string) bool {
	return strings.HasPrefix(key, "sk-")
}
