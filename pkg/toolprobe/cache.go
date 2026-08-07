package toolprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache stores signed probe receipts keyed by Identity.Key().
type Cache interface {
	Get(id Identity) (Receipt, bool)
	Put(r Receipt) error
}

// MemoryCache is an in-process cache for tests and short-lived processes.
type MemoryCache struct {
	mu    sync.Mutex
	items map[string]Receipt
}

// NewMemoryCache returns an empty memory cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{items: map[string]Receipt{}}
}

func (c *MemoryCache) Get(id Identity) (Receipt, bool) {
	if c == nil {
		return Receipt{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.items[id.Key()]
	return r, ok
}

func (c *MemoryCache) Put(r Receipt) error {
	if c == nil {
		return fmt.Errorf("toolprobe: nil cache")
	}
	if err := r.Identity.Valid(); err != nil {
		return err
	}
	if err := r.VerifySignature(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]Receipt{}
	}
	c.items[r.Identity.Key()] = r
	return nil
}

// FileCache is a durable JSON map at Path. Reads are whole-file; writes are
// rename-atomic. Concurrent writers should not share one path without an
// external lock; production uses one process owning the fleet probe cache.
type FileCache struct {
	Path string
	mu   sync.Mutex
}

// DefaultCachePath is the repo-relative probe receipt store.
const DefaultCachePath = ".herd/toolprobe-cache.json"

// NewFileCache returns a file-backed cache. Empty path uses DefaultCachePath.
func NewFileCache(path string) *FileCache {
	if path == "" {
		path = DefaultCachePath
	}
	return &FileCache{Path: path}
}

type filePayload struct {
	Version int                `json:"version"`
	Items   map[string]Receipt `json:"items"`
}

func (c *FileCache) load() (filePayload, error) {
	var p filePayload
	b, err := os.ReadFile(c.Path)
	if os.IsNotExist(err) {
		return filePayload{Version: SchemaVersion, Items: map[string]Receipt{}}, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("toolprobe cache corrupt: %w", err)
	}
	if p.Items == nil {
		p.Items = map[string]Receipt{}
	}
	return p, nil
}

func (c *FileCache) Get(id Identity) (Receipt, bool) {
	if c == nil || c.Path == "" {
		return Receipt{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.load()
	if err != nil {
		return Receipt{}, false
	}
	r, ok := p.Items[id.Key()]
	return r, ok
}

func (c *FileCache) Put(r Receipt) error {
	if c == nil || c.Path == "" {
		return fmt.Errorf("toolprobe: cache path required")
	}
	if err := r.Identity.Valid(); err != nil {
		return err
	}
	if err := r.VerifySignature(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	p, err := c.load()
	if err != nil {
		return err
	}
	p.Version = SchemaVersion
	if p.Items == nil {
		p.Items = map[string]Receipt{}
	}
	p.Items[r.Identity.Key()] = r
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.Path), ".toolprobe-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, c.Path)
}

// LookupFresh returns a fresh, signature-valid receipt for id, if any.
func LookupFresh(cache Cache, id Identity, now time.Time) (Receipt, bool) {
	if cache == nil {
		return Receipt{}, false
	}
	r, ok := cache.Get(id)
	if !ok {
		return Receipt{}, false
	}
	if err := r.VerifySignature(); err != nil {
		return Receipt{}, false
	}
	if !r.Identity.Matches(id) {
		return Receipt{}, false
	}
	if !r.Fresh(now) {
		return Receipt{}, false
	}
	return r, true
}
