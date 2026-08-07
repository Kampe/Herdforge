package signerboundary

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DurableNonceLedger is a crash-durable single-use nonce store for the signer.
// Memory-only stores allow replay after restart (FAC-169 causal warning §a).
type DurableNonceLedger struct {
	mu   sync.Mutex
	path string
	seen map[string]struct{}
}

// NewDurableNonceLedger opens or creates a ledger at path (0600).
func NewDurableNonceLedger(path string) (*DurableNonceLedger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("nonce ledger path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &DurableNonceLedger{path: path, seen: make(map[string]struct{})}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *DurableNonceLedger) load() error {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// format: nonce\tunix_ts
		parts := strings.SplitN(line, "\t", 2)
		if parts[0] != "" {
			l.seen[parts[0]] = struct{}{}
		}
	}
	return sc.Err()
}

// Accept returns false if nonce was already used. On true, persists the nonce
// with fsync before returning (crash-durable single-use).
func (l *DurableNonceLedger) Accept(nonce string) bool {
	if l == nil || strings.TrimSpace(nonce) == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[nonce]; ok {
		return false
	}
	// Append + fsync before marking in memory as durable.
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	line := fmt.Sprintf("%s\t%d\n", nonce, time.Now().Unix())
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return false
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false
	}
	if err := f.Close(); err != nil {
		return false
	}
	// Directory fsync for rename durability of the file itself on some FS.
	_ = syncDir(filepath.Dir(l.path))
	l.seen[nonce] = struct{}{}
	return true
}
