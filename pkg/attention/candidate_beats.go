package attention

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// RecordCandidateBeat commits one complete observation under a cross-process
// lock. Callers must not call it for a partial/failed evidence read: doing so
// would forget prior findings and conceal escalation on the next healthy beat.
func RecordCandidateBeat(path string, items []CandidateItem) ([]CandidateItem, error) {
	var result []CandidateItem
	err := envelope.WithSessionFileLock(path, func() error {
		previous := map[string]int{}
		body, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal(body, &previous); err != nil {
				return fmt.Errorf("candidate beat state is corrupt: %w", err)
			}
			if previous == nil {
				return fmt.Errorf("candidate beat state is null")
			}
			for key, count := range previous {
				if key == "" || count < 1 {
					return fmt.Errorf("candidate beat state has invalid counter")
				}
			}
		}
		next := map[string]int{}
		result = make([]CandidateItem, 0, len(items))
		for _, item := range items {
			updated, key := EscalateCandidate(item, previous)
			if _, duplicate := next[key]; duplicate {
				return fmt.Errorf("duplicate candidate in attention beat")
			}
			if updated.Beats < 1 {
				return fmt.Errorf("candidate beat counter overflow")
			}
			next[key] = updated.Beats
			result = append(result, updated)
		}
		body, err = json.Marshal(next)
		if err != nil {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".attention-beat-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.Write(body); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return err
		}
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	})
	if err != nil {
		// Evidence remains useful even when its escalation state cannot be saved.
		return items, err
	}
	return result, nil
}
