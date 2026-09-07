package sync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
)

func historyReceipt(candidate string) *CompletionReceipt {
	r := &CompletionReceipt{RepoID: "repo", TaskRef: "FAC-761", CandidateSHA: candidate, Verdict: "PASS"}
	r.Seal()
	return r
}

func TestReceiptHistoryPreservesBytesAndRequiresExplicitPrior(t *testing.T) {
	dir := t.TempDir()
	old := historyReceipt("old")
	next := historyReceipt("next")
	if err := WriteReceipt(dir, old); err != nil {
		t.Fatal(err)
	}
	path := ReceiptPath(dir, old.TaskRef)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve authentic bytes, including insignificant original formatting.
	original = append(original, '\n')
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceipt(dir, next); err == nil {
		t.Fatal("ordinary write replaced receipt")
	}
	if err := SupersedeReceipt(dir, next, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong prior replaced receipt")
	}
	back, _ := os.ReadFile(path)
	if !bytes.Equal(back, original) {
		t.Fatal("refusal changed receipt")
	}
	if err := SupersedeReceipt(dir, next, old.Digest); err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(filepath.Join(receiptHistoryDir(dir, old.TaskRef), old.Digest+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archived, original) {
		t.Fatal("old bytes were not preserved")
	}
	if err := SupersedeReceipt(dir, next, old.Digest); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := SupersedeReceipt(dir, next, strings.Repeat("1", 64)); err == nil {
		t.Fatal("matching current accepted unrelated prior")
	}
}

func TestReceiptHistoryHasOneConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	old := historyReceipt("old")
	if err := WriteReceipt(dir, old); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg stdsync.WaitGroup
	for _, candidate := range []string{"a", "b"} {
		wg.Add(1)
		go func(candidate string) {
			defer wg.Done()
			<-start
			results <- SupersedeReceipt(dir, historyReceipt(candidate), old.Digest)
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("got %d winners", successes)
	}
	r, err := LoadReceipt(ReceiptPath(dir, old.TaskRef))
	if err != nil {
		t.Fatal(err)
	}
	if r.Digest != r.ComputeDigest() || (r.CandidateSHA != "a" && r.CandidateSHA != "b") {
		t.Fatalf("invalid published receipt: %+v", r)
	}
}

func TestReceiptHistoryRecoversEveryDurableBoundary(t *testing.T) {
	for _, stage := range []string{"history", "decision", "published"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			old := historyReceipt("old")
			next := historyReceipt("next")
			if err := WriteReceipt(dir, old); err != nil {
				t.Fatal(err)
			}
			interrupted := errors.New("process stopped")
			err := persistReceipt(dir, next, old.Digest, func(at string) error {
				if at == stage {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("did not exercise %s boundary: %v", stage, err)
			}
			current, err := LoadReceipt(ReceiptPath(dir, old.TaskRef))
			if err != nil {
				t.Fatal(err)
			}
			want := old.Digest
			if stage == "published" {
				want = next.Digest
			}
			if current.Digest != want || current.Digest != current.ComputeDigest() {
				t.Fatal("partial or wrong current receipt")
			}
			if stage != "history" {
				if err := SupersedeReceipt(dir, historyReceipt("competitor"), old.Digest); err == nil {
					t.Fatal("competing successor replaced durable decision")
				}
			}
			if err := SupersedeReceipt(dir, next, old.Digest); err != nil {
				t.Fatalf("resume: %v", err)
			}
		})
	}
}

func TestReceiptHistoryRefusesTamperingAndIdentityChanges(t *testing.T) {
	for _, kind := range []string{"seal", "repo", "task-id", "missing-task-id", "history"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			old := historyReceipt("old")
			old.TaskID = "opaque-a"
			old.Seal()
			next := historyReceipt("next")
			next.TaskID = "opaque-a"
			if err := WriteReceipt(dir, old); err != nil {
				t.Fatal(err)
			}
			path := ReceiptPath(dir, old.TaskRef)
			switch kind {
			case "seal":
				b, _ := os.ReadFile(path)
				b = bytes.Replace(b, []byte(`"old"`), []byte(`"forged"`), 1)
				if err := os.WriteFile(path, b, 0o644); err != nil {
					t.Fatal(err)
				}
			case "repo":
				next.RepoID = "other"
			case "task-id":
				next.TaskID = "opaque-b"
			case "missing-task-id":
				next.TaskID = ""
			case "history":
				h := receiptHistoryDir(dir, old.TaskRef)
				if err := os.MkdirAll(h, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h, old.Digest+".json"), []byte("bad"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			if err := SupersedeReceipt(dir, next, old.Digest); err == nil {
				t.Fatal("invalid transition admitted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal mutated current")
			}
		})
	}
}
