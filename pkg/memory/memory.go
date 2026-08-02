package memory

import (
	"encoding/json"
	"fmt"
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

	dir := filepath.Join(m.MemoryDir, "errors", domain)
	if err := os.MkdirAll(dir, 0755); err != nil {
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

	filePath := filepath.Join(dir, fmt.Sprintf("%s.json", slug))
	data, err := json.MarshalIndent(pat, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal error pattern: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write error pattern file: %w", err)
	}

	return pat, nil
}

func (m *MemoryStore) QueryRelevantPatterns(query string) ([]*ErrorPattern, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	errorsDir := filepath.Join(m.MemoryDir, "errors")
	if _, err := os.Stat(errorsDir); os.IsNotExist(err) {
		return nil, nil
	}

	var matched []*ErrorPattern
	err := filepath.Walk(errorsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
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
