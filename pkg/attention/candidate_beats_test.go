package attention

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCandidateBeatPersistsAndSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beats.json")
	item, _ := ClassifyCandidate(readyCandidateFixture())
	first, err := RecordCandidateBeat(path, []CandidateItem{item})
	if err != nil || first[0].Beats != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RecordCandidateBeat(path, []CandidateItem{item}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	last, err := RecordCandidateBeat(path, []CandidateItem{item})
	if err != nil || last[0].Beats != 6 || !last[0].Escalated {
		t.Fatalf("lost serialized beat: %+v %v", last, err)
	}
	if _, err := RecordCandidateBeat(path, nil); err != nil {
		t.Fatal(err)
	}
	fresh, err := RecordCandidateBeat(path, []CandidateItem{item})
	if err != nil || fresh[0].Beats != 1 {
		t.Fatalf("settled condition did not reset: %+v %v", fresh, err)
	}
}
func TestCandidateBeatCorruptionIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beats.json")
	const corrupt = "{broken"
	if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	item, _ := ClassifyCandidate(readyCandidateFixture())
	observed, err := RecordCandidateBeat(path, []CandidateItem{item})
	if err == nil {
		t.Fatal("corrupt state silently reset")
	}
	if len(observed) != 1 || observed[0].SHA != item.SHA {
		t.Fatal("state-write failure erased collected evidence")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != corrupt {
		t.Fatalf("corrupt evidence overwritten: %q %v", body, err)
	}
}
