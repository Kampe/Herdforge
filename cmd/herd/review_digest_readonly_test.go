package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestVerdictDigestQueryDoesNotCreateState(t *testing.T) {
	binary := buildHerd(t)
	sha := strings.Repeat("a", 40)
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "existing"}[existing], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".herd", "review-ledger.jsonl")
			original := []byte(`{"event":"verdict","sha":"` + sha + `","reviewer":"r1","verdict":"FAIL"}` + "\n")
			if existing {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(binary, "review-ledger", "verdict-digest", sha, "r1")
			cmd.Dir = root
			cmd.Env = append(reviewTestEnv(), "HERD_REVIEW_LEDGER="+path)
			out, err := cmd.CombinedOutput()
			if existing && err != nil {
				t.Fatalf("query existing verdict: %v %s", err, out)
			}
			if !existing && err == nil {
				t.Fatal("missing verdict reported success")
			}
			if existing {
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(original) {
					t.Fatal("query modified ledger")
				}
			} else if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatal("missing query created ledger directory")
			}
			if _, err := os.Stat(reviewledger.QueuePathFor(path)); !os.IsNotExist(err) {
				t.Fatal("query created harvest queue")
			}
		})
	}
}
