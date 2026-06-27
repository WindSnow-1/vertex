package nodes

import (
	"encoding/json"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"vertex/internal/config"
)

type Node struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	RawURI   string `json:"raw_uri"`
	Disabled bool   `json:"disabled"`
}

type NodeHealth struct {
	SuccessCount        int     `json:"success_count"`
	FailCount           int     `json:"fail_count"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTestMs          float64 `json:"last_test_ms"`
	LastTestError       string  `json:"last_test_error"`
	LastSuccessAt       int64   `json:"last_success_at"`
	LastFailAt          int64   `json:"last_fail_at"`
	CooldownUntil       int64   `json:"cooldown_until"`
}

var (
	mu        sync.Mutex
	nodeList  []Node
	healthMap = make(map[string]*NodeHealth)
	loaded    bool

	OnDeleteNode func(uri string)
)

func fileDir() string {
	return "config"
}

func ensureLoaded() {
	if loaded {
		return
	}
	loaded = true
	if b, err := os.ReadFile(filepath.Join(fileDir(), "nodes.json")); err == nil {
		var d struct {
			Nodes []Node `json:"nodes"`
		}
		_ = json.Unmarshal(b, &d)
		nodeList = d.Nodes
	}
	if b, err := os.ReadFile(filepath.Join(fileDir(), "node_health.json")); err == nil {
		_ = json.Unmarshal(b, &healthMap)
	}
}

func LoadNodes() []Node {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	return nodeList
}

func LoadHealth() map[string]*NodeHealth {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	return healthMap
}

func GetNodeName(uri string) string {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	for _, n := range nodeList {
		if n.RawURI == uri {
			return n.Name
		}
	}
	return "Unknown"
}

func saveNodesUnsafe() {
	d := map[string]any{"nodes": nodeList}
	b, _ := json.MarshalIndent(d, "", "  ")
	dir := fileDir()
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, "nodes.json"), b, 0644)
}

func saveHealthUnsafe() {
	b, _ := json.MarshalIndent(healthMap, "", "  ")
	dir := fileDir()
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, "node_health.json"), b, 0644)
}

func MergeNodes(newNodes []Node) int {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	existing := make(map[string]bool)
	for _, n := range nodeList {
		existing[n.RawURI] = true
	}
	added := 0
	for _, n := range newNodes {
		if !existing[n.RawURI] {
			nodeList = append(nodeList, n)
			existing[n.RawURI] = true
			added++
		}
	}
	saveNodesUnsafe()
	return added
}

func DeleteNode(uri string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	var kept []Node
	for _, n := range nodeList {
		if n.RawURI != uri {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	delete(healthMap, uri)
	saveNodesUnsafe()
	saveHealthUnsafe()
	if OnDeleteNode != nil {
		OnDeleteNode(uri)
	}
}

func BatchUpdateDisabled(uris []string, disabled bool) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
	}
	for i, n := range nodeList {
		if targets[n.RawURI] {
			nodeList[i].Disabled = disabled
		}
	}
	saveNodesUnsafe()
}

func BatchDelete(uris []string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
		delete(healthMap, u)
	}
	var kept []Node
	for _, n := range nodeList {
		if !targets[n.RawURI] {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	if OnDeleteNode != nil {
		for _, u := range uris {
			OnDeleteNode(u)
		}
	}
}

func DedupNodes() int {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	seen := make(map[string]bool)
	var kept []Node
	removed := 0
	for _, n := range nodeList {
		if !seen[n.RawURI] {
			seen[n.RawURI] = true
			kept = append(kept, n)
		} else {
			removed++
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	return removed
}

func DeleteDisabled() int {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	var kept []Node
	removed := 0
	for _, n := range nodeList {
		if !n.Disabled {
			kept = append(kept, n)
		} else {
			removed++
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	return removed
}

func EnableNode(uri string) bool {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	found := false
	for i, n := range nodeList {
		if n.RawURI == uri {
			nodeList[i].Disabled = false
			if h, ok := healthMap[uri]; ok {
				h.CooldownUntil = 0
			}
			found = true
			break
		}
	}
	if found {
		saveNodesUnsafe()
		saveHealthUnsafe()
	}
	return found
}

func RecordTest(uri string, ok bool, ms float64, errStr string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	h, exists := healthMap[uri]
	if !exists {
		h = &NodeHealth{}
		healthMap[uri] = h
	}
	h.LastTestMs = ms
	h.LastTestError = errStr
	if ok {
		h.SuccessCount++
		h.ConsecutiveFailures = 0
		h.LastSuccessAt = time.Now().Unix()
		h.CooldownUntil = 0
	} else {
		h.FailCount++
		h.ConsecutiveFailures++
		h.LastFailAt = time.Now().Unix()
		failures := h.ConsecutiveFailures
		if failures < 1 {
			failures = 1
		}
		exp := failures - 1
		if exp > 6 {
			exp = 6
		}
		cooldown := 30 * (1 << exp)
		if cooldown > 1800 {
			cooldown = 1800
		}
		h.CooldownUntil = time.Now().Unix() + int64(cooldown)
	}
	saveHealthUnsafe()
}

type scoredNode struct {
	node  Node
	score float64
}

func SelectForParallel(k int) []Node {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	now := time.Now().Unix()
	var scored []scoredNode
	var cooldownNodes []scoredNode

	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil && h.CooldownUntil > now {
			cooldownNodes = append(cooldownNodes, scoredNode{n, float64(h.CooldownUntil)})
			continue
		}
		score := 100.0
		if h != nil {
			score += math.Min(float64(h.SuccessCount), 100) * 3
			score -= math.Min(float64(h.FailCount), 100) * 4
			score -= float64(h.ConsecutiveFailures) * 25
			if h.LastTestMs > 0 {
				score -= math.Min(h.LastTestMs/1000.0, 30.0)
			}
			lastSeen := h.LastSuccessAt
			if h.LastFailAt > lastSeen {
				lastSeen = h.LastFailAt
			}
			if lastSeen == 0 {
				score += 20
			} else if now-lastSeen > 3600 {
				score += 10
			}
		} else {
			score += 20
		}
		if score < 1 {
			score = 1
		}
		scored = append(scored, scoredNode{n, score})
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })

	if len(scored) < k && len(cooldownNodes) > 0 {
		sort.Slice(cooldownNodes, func(i, j int) bool { return cooldownNodes[i].score < cooldownNodes[j].score })
		needed := k - len(scored)
		if needed > len(cooldownNodes) {
			needed = len(cooldownNodes)
		}
		scored = append(scored, cooldownNodes[:needed]...)
	}

	topK := config.Load().ParallelNodeTopK
	if topK <= 0 {
		topK = 80
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}

	weights := make([]float64, len(scored))
	totalWeight := 0.0
	for i, s := range scored {
		w := s.score + 120.0
		if w < 1 {
			w = 1
		}
		weights[i] = w
		totalWeight += w
	}

	var selected []Node
	for i := 0; i < k && len(scored) > 0; i++ {
		r := rand.Float64() * totalWeight
		idx := len(weights) - 1
		for j, w := range weights {
			r -= w
			if r <= 0 {
				idx = j
				break
			}
		}
		selected = append(selected, scored[idx].node)
		totalWeight -= weights[idx]
		weights = append(weights[:idx], weights[idx+1:]...)
		scored = append(scored[:idx], scored[idx+1:]...)
	}

	log.Printf("[nodes] selected %d/%d for parallel", len(selected), k)
	return selected
}
