package memory

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ErrorPattern struct {
	ID         string    `json:"id"`
	Domain     string    `json:"domain"`
	Slug       string    `json:"slug"`
	Summary    string    `json:"summary"`
	FixDetails string    `json:"fix_details"`
	CreatedAt  time.Time `json:"created_at"`
}

type MemoryStore struct {
	mu        sync.RWMutex
	MemoryDir string
}

func NewMemoryStore(memoryDir string) *MemoryStore {
	return &MemoryStore{MemoryDir: memoryDir}
}

func (m *MemoryStore) RecordErrorPattern(domain, slug, summary, fix string) (*ErrorPattern, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	errorsDir := filepath.Join(m.MemoryDir, "errors")
	if err := os.MkdirAll(errorsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create memory domain dir: %w", err)
	}

	root, err := os.OpenRoot(errorsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open memory errors root: %w", err)
	}
	defer root.Close()

	if err := root.MkdirAll(domain, 0755); err != nil {
		return nil, fmt.Errorf("failed to create memory domain dir: %w", err)
	}

	pat := &ErrorPattern{
		ID:         fmt.Sprintf("%s-%s", domain, slug),
		Domain:     domain,
		Slug:       slug,
		Summary:    summary,
		FixDetails: fix,
		CreatedAt:  time.Now(),
	}

	filePath := filepath.Join(domain, fmt.Sprintf("%s.json", slug))
	data, err := json.MarshalIndent(pat, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal error pattern: %w", err)
	}

	if err := root.WriteFile(filePath, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write error pattern file: %w", err)
	}

	return pat, nil
}

func (m *MemoryStore) QueryRelevantPatterns(query string) ([]*ErrorPattern, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	errorsDir := filepath.Join(m.MemoryDir, "errors")
	root, err := os.OpenRoot(errorsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open memory errors root: %w", err)
	}
	defer root.Close()

	var matched []*ErrorPattern
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}

		data, err := root.ReadFile(path)
		if err != nil {
			return nil
		}

		var pat ErrorPattern
		if err := json.Unmarshal(data, &pat); err == nil {
			lowerQuery := strings.ToLower(query)
			if strings.Contains(strings.ToLower(pat.Summary), lowerQuery) ||
				strings.Contains(strings.ToLower(pat.Domain), lowerQuery) ||
				strings.Contains(strings.ToLower(pat.Slug), lowerQuery) {
				matched = append(matched, &pat)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("error walking memory store: %w", err)
	}

	return matched, nil
}
