package spotify

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
)

// BPMCache persists track→BPM mappings to disk.
type BPMCache struct {
	mu      sync.Mutex
	path    string
	entries map[string]float64 // "track — artist" → BPM
}

func LoadBPMCache(path string) *BPMCache {
	c := &BPMCache{path: path, entries: make(map[string]float64)}

	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	json.Unmarshal(data, &c.entries)
	log.Printf("Loaded %d BPM entries from cache", len(c.entries))
	return c
}

func (c *BPMCache) key(track, artist string) string {
	return strings.ToLower(track + " — " + artist)
}

func (c *BPMCache) Get(track, artist string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(track, artist)]
}

func (c *BPMCache) Set(track, artist string, bpm float64) {
	c.mu.Lock()
	c.entries[c.key(track, artist)] = bpm
	c.mu.Unlock()
	c.save()
}

func (c *BPMCache) save() {
	c.mu.Lock()
	data, _ := json.MarshalIndent(c.entries, "", "  ")
	path := c.path
	c.mu.Unlock()
	os.WriteFile(path, data, 0600)
}

// All returns all cached entries.
func (c *BPMCache) All() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := make(map[string]float64, len(c.entries))
	for k, v := range c.entries {
		copy[k] = v
	}
	return copy
}
