package metrics

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type RequestRecord struct {
	Time    int64  `json:"time"`
	Path    string `json:"path"`
	Success bool   `json:"success"`
	Latency int64  `json:"latency"`
}

type Collector struct {
	startTime int64
	total     atomic.Int64
	success   atomic.Int64
	fail      atomic.Int64
	active    atomic.Int64
	u429      atomic.Int64
	uEmpty    atomic.Int64
	uAuth     atomic.Int64

	mu      sync.Mutex
	history []RequestRecord
}

func New() *Collector {
	return &Collector{startTime: time.Now().Unix()}
}

func (c *Collector) IncTotal()       { c.total.Add(1) }
func (c *Collector) IncSuccess()     { c.success.Add(1) }
func (c *Collector) IncFail()        { c.fail.Add(1) }
func (c *Collector) IncActive()      { c.active.Add(1) }
func (c *Collector) DecActive()      { c.active.Add(-1) }
func (c *Collector) IncUpstream429() { c.u429.Add(1) }
func (c *Collector) IncUpstreamEmpty() { c.uEmpty.Add(1) }
func (c *Collector) IncUpstreamAuth() { c.uAuth.Add(1) }

func (c *Collector) Record(path string, success bool, latencyMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := RequestRecord{
		Time:    time.Now().Unix(),
		Path:    path,
		Success: success,
		Latency: latencyMs,
	}
	c.history = append(c.history, rec)
	if len(c.history) > 200 {
		c.history = c.history[len(c.history)-200:]
	}
}

func (c *Collector) RecentRequests() []RequestRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RequestRecord, len(c.history))
	copy(out, c.history)
	return out
}

func (c *Collector) Reset() {
	c.total.Store(0)
	c.success.Store(0)
	c.fail.Store(0)
	c.u429.Store(0)
	c.uEmpty.Store(0)
	c.uAuth.Store(0)
	c.mu.Lock()
	c.history = nil
	c.mu.Unlock()
}

func (c *Collector) Stats() map[string]any {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	total := c.total.Load()
	succ := c.success.Load()
	var rate float64
	if total > 0 {
		rate = float64(succ) / float64(total) * 100
	}

	return map[string]any{
		"status": "running",
		"memory": map[string]any{
			"alloc_mb":   float64(mem.Alloc) / 1048576,
			"heap_inuse": mem.HeapInuse,
			"num_gc":     mem.NumGC,
			"goroutines": runtime.NumGoroutine(),
		},
		"requests": map[string]any{
			"total":          total,
			"success":        succ,
			"fail":           c.fail.Load(),
			"active":         c.active.Load(),
			"success_rate":   rate,
			"upstream_429":   c.u429.Load(),
			"upstream_empty": c.uEmpty.Load(),
			"upstream_auth":  c.uAuth.Load(),
		},
	}
}
