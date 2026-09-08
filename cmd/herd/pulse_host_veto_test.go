package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/pulse"
)

// TestLoadReapEvidenceCrossHostDissentKeepsAwaitingVerdict is the reachable
// production consumer named by the 52bb W4 FAIL: pulse loadReapEvidence →
// review.Vetoed → applyReapEvidence AwaitingVerdict.
func TestLoadReapEvidenceCrossHostDissentKeepsAwaitingVerdict(t *testing.T) {
	reviewer := "review-fac-765-52bbffb7dc79"

	t.Run("hostA_FAIL_then_hostB_PASS", func(t *testing.T) {
		dir, sha := committedLane(t)
		writeHostVerdictLedger(t, sha, reviewer,
			hostVerdictRow{"host-a", "FAIL"},
			hostVerdictRow{"host-b", "PASS"},
		)
		if !awaitingVerdictForLane(t, dir) {
			t.Fatal("host-A FAIL then host-B PASS must keep AwaitingVerdict")
		}
	})
	t.Run("reversed_hostB_PASS_then_hostA_FAIL", func(t *testing.T) {
		dir, sha := committedLane(t)
		writeHostVerdictLedger(t, sha, reviewer,
			hostVerdictRow{"host-b", "PASS"},
			hostVerdictRow{"host-a", "FAIL"},
		)
		if !awaitingVerdictForLane(t, dir) {
			t.Fatal("host-B PASS then host-A FAIL must keep AwaitingVerdict")
		}
	})
	t.Run("same_host_FAIL_then_PASS_reassessment", func(t *testing.T) {
		dir, sha := committedLane(t)
		writeHostVerdictLedger(t, sha, reviewer,
			hostVerdictRow{"host-a", "FAIL"},
			hostVerdictRow{"host-a", "PASS"},
		)
		if awaitingVerdictForLane(t, dir) {
			t.Fatal("same-host FAIL then PASS must clear AwaitingVerdict")
		}
	})
}

type hostVerdictRow struct {
	host, verdict string
}

func committedLane(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "reap@test")
	runGitT(t, dir, "config", "user.name", "reap")
	runGitT(t, dir, "config", "core.hooksPath", "/dev/null")
	runGitT(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "base.txt", "base\n")
	baseSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseSHA)
	runGitT(t, dir, "commit", "--allow-empty", "-m", "feat: real work")
	sha = strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	return dir, sha
}

func writeHostVerdictLedger(t *testing.T, sha, reviewer string, vs ...hostVerdictRow) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, v := range vs {
		row := map[string]string{
			"event":    "verdict",
			"sha":      sha,
			"reviewer": reviewer,
			"verdict":  v.verdict,
		}
		if v.host != "" {
			row["host"] = v.host
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", path)
}

func awaitingVerdictForLane(t *testing.T, dir string) bool {
	t.Helper()
	entries := []herdr.AgentEntry{{Name: "task-fac-765", Cwd: dir}}
	ev := loadReapEvidence(context.Background(), entries, nil)
	if ev.ledgerCorrupt {
		t.Fatalf("ledger must be readable: %+v", ev)
	}
	agent := applyReapEvidence(pulse.AgentObservation{Name: "task-fac-765"}, "FAC-765", ev)
	if !agent.CommittedWork {
		t.Fatal("expected CommittedWork so AwaitingVerdict is the KEEP signal under test")
	}
	return agent.AwaitingVerdict
}
