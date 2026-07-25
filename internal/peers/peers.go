// Package peers caches the devices we have seen, so a send does not have to
// wait for a full discovery round every time.
package peers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
)

const (
	fileName = "peers.json"

	// maxAge drops devices we have not seen in a long time.
	maxAge = 30 * 24 * time.Hour
)

// Entry is a cached device with the time it was last seen.
type Entry struct {
	proto.Device
	LastSeen time.Time `json:"lastSeen"`
}

// Cache is the on-disk peer cache. It is safe for concurrent use.
type Cache struct {
	path string

	mu      sync.Mutex
	entries map[string]Entry
	dirty   bool
}

// Load reads the cache. A missing or damaged file yields an empty cache rather
// than an error: it is only an optimisation.
func Load() *Cache {
	cache := &Cache{entries: map[string]Entry{}}

	dir, err := identity.ConfigDir()
	if err != nil {
		return cache
	}
	cache.path = filepath.Join(dir, fileName)

	data, err := os.ReadFile(cache.path)
	if err != nil {
		return cache
	}
	var stored []Entry
	if err := json.Unmarshal(data, &stored); err != nil {
		return cache
	}
	for _, entry := range stored {
		if time.Since(entry.LastSeen) > maxAge {
			continue
		}
		cache.entries[key(entry.Device)] = entry
	}
	return cache
}

// Put records a device as seen now.
func (c *Cache) Put(device proto.Device) {
	if device.Alias == "" || device.IP == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	existing, ok := c.entries[key(device)]
	if ok && existing.Device == device {
		// Same data; only the timestamp moves, no need to rewrite the file
		// for every announcement.
		existing.LastSeen = time.Now()
		c.entries[key(device)] = existing
		return
	}
	c.entries[key(device)] = Entry{Device: device, LastSeen: time.Now()}
	c.dirty = true
}

// Lookup returns the cached devices whose alias matches, most recently seen
// first. Exact matches (case-insensitive) win over prefix matches.
func (c *Cache) Lookup(alias string) []proto.Device {
	c.mu.Lock()
	defer c.mu.Unlock()

	var exact, prefix []Entry
	for _, entry := range c.entries {
		switch {
		case strings.EqualFold(entry.Alias, alias):
			exact = append(exact, entry)
		case strings.HasPrefix(strings.ToLower(entry.Alias), strings.ToLower(alias)):
			prefix = append(prefix, entry)
		}
	}
	return devicesOf(append(byRecency(exact), byRecency(prefix)...))
}

// All returns every cached device, most recently seen first.
func (c *Cache) All() []proto.Device {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	return devicesOf(byRecency(entries))
}

// Save writes the cache if anything changed.
func (c *Cache) Save() error {
	c.mu.Lock()
	if !c.dirty || c.path == "" {
		c.mu.Unlock()
		return nil
	}
	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	c.dirty = false
	c.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Alias < entries[j].Alias })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, append(data, '\n'), 0o600)
}

// key identifies a device: the fingerprint if it has one, else its address.
func key(device proto.Device) string {
	if device.Fingerprint != "" {
		return device.Fingerprint
	}
	return device.IP + ":" + strings.ToLower(device.Alias)
}

func byRecency(entries []Entry) []Entry {
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastSeen.After(entries[j].LastSeen) })
	return entries
}

func devicesOf(entries []Entry) []proto.Device {
	devices := make([]proto.Device, 0, len(entries))
	for _, entry := range entries {
		devices = append(devices, entry.Device)
	}
	return devices
}
