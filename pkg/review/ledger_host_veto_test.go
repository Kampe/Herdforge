package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVetoedKeepsCrossHostDissent(t *testing.T) {
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reviewer := "review-fac-765-52bbffb7dc79"

	t.Run("hostA_FAIL_then_hostB_PASS", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"host-a", "FAIL"},
			hostVerdict{"host-b", "PASS"},
		)
		assertSHAVetoed(t, l, sha, true)
	})
	t.Run("reversed_hostB_PASS_then_hostA_FAIL", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"host-b", "PASS"},
			hostVerdict{"host-a", "FAIL"},
		)
		assertSHAVetoed(t, l, sha, true)
	})
	t.Run("hostA_BLOCKED_then_hostB_PASS", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"host-a", "BLOCKED"},
			hostVerdict{"host-b", "PASS"},
		)
		assertSHAVetoed(t, l, sha, true)
	})
	t.Run("same_host_FAIL_then_PASS_reassessment", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"host-a", "FAIL"},
			hostVerdict{"host-a", "PASS"},
		)
		assertSHAVetoed(t, l, sha, false)
	})
	t.Run("legacy_empty_host_FAIL_then_PASS", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"", "FAIL"},
			hostVerdict{"", "PASS"},
		)
		assertSHAVetoed(t, l, sha, false)
	})
	t.Run("independent_hosts_both_PASS", func(t *testing.T) {
		l := ledgerWithHostVerdicts(t, sha, reviewer,
			hostVerdict{"host-a", "PASS"},
			hostVerdict{"host-b", "PASS"},
		)
		assertSHAVetoed(t, l, sha, false)
	})
}

type hostVerdict struct {
	host, verdict string
}

func ledgerWithHostVerdicts(t *testing.T, sha, reviewer string, vs ...hostVerdict) *Ledger {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	writeHostJSONL(t, path, sha, reviewer, "", vs...)
	return OpenLedger(path)
}

func writeHostJSONL(t *testing.T, path, sha, reviewer, ref string, vs ...hostVerdict) {
	t.Helper()
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
		if ref != "" {
			row["branch"] = "task/" + ref
			row["artifact"] = ref + ".md"
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSHAVetoed(t *testing.T, l *Ledger, sha string, want bool) {
	t.Helper()
	ctx := context.Background()
	veto, err := l.Vetoed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := veto[sha]; got != want {
		t.Fatalf("Vetoed[%s]=%v want %v (map=%v)", sha, got, want, veto)
	}
	snap, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Vetoed()[sha]; got != want {
		t.Fatalf("Snapshot.Vetoed[%s]=%v want %v", sha, got, want)
	}
}
