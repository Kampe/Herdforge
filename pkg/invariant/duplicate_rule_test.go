package invariant

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestNoNewDuplicatedRules is the FAC-575 gate.
//
// Six defects in one session were the same root cause: a rule implemented twice,
// the copies diverging, and the fix landing on whichever copy was in front of me
// (FAC-562, 565, 569, 571, 573, 574). Every one was found by a consumer.
//
// The prevention cannot be an intention to check for a second copy, because that
// intention was present and failed six times. This is the mechanical version.
//
// To accept a genuinely necessary duplicate, run the baseline regenerator below
// and say WHY in the commit message. The point is that duplication becomes a
// deliberate, reviewed act rather than an invisible default.
func TestNoNewDuplicatedRules(t *testing.T) {
	root := repoRoot(t)
	found, err := FindDuplicateLiterals(root, []string{"pkg", "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := LoadBaseline(filepath.Join(root, BaselineFile))
	if err != nil {
		t.Fatal(err)
	}
	violations := NewViolations(found, base)
	if len(violations) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("new duplicated rule(s) — the same decision is now written in more than one place.\n")
	b.WriteString("This is the root cause of FAC-562, 565, 569, 571, 573 and 574: the copies diverge\n")
	b.WriteString("and a fix lands on only one of them.\n\n")
	for _, v := range violations {
		b.WriteString(Describe(v) + "\n")
	}
	b.WriteString("\nExtract one definition both callers use (see pkg/refname for the shape),\n")
	b.WriteString("or regenerate the baseline with HERD_WRITE_DUPLICATE_BASELINE=1 and justify it.\n")
	t.Fatal(b.String())
}

// TestRegenerateBaseline is the explicit escape hatch. It only writes when asked,
// so a normal run can never silently absorb a new duplicate.
func TestRegenerateBaseline(t *testing.T) {
	if os.Getenv("HERD_WRITE_DUPLICATE_BASELINE") != "1" {
		t.Skip("set HERD_WRITE_DUPLICATE_BASELINE=1 to record current duplicates as inherited")
	}
	root := repoRoot(t)
	found, err := FindDuplicateLiterals(root, []string{"pkg", "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseline(filepath.Join(root, BaselineFile),
		time.Now().UTC().Format(time.RFC3339), found); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d inherited duplicate(s)", len(found))
}

// The detector must catch the exact shapes that got through, including the two
// that were NOT identical literals.
func TestDetectorCatchesTheKnownShapes(t *testing.T) {
	for _, s := range []string{
		"review-ledger.jsonl",     // FAC-565: filename
		"--prompt-interactive",    // FAC-573: CLI flag
		"harvest/",                // FAC-574: ref prefix
		strings.Repeat("policy ", 8), // FAC-571: long message
	} {
		if !Distinctive(s) {
			t.Fatalf("detector must treat %q as distinctive", s)
		}
	}
	// And must NOT flag ordinary short words, or the gate becomes noise.
	for _, s := range []string{"json", "main", "done", "ok"} {
		if Distinctive(s) {
			t.Fatalf("detector must ignore the bare word %q", s)
		}
	}
	// A prefix key is derived for path-like values (the non-identical case).
	keys := indexKeys("harvest/%s-%s")
	joined := strings.Join(keys, ",")
	if !strings.Contains(joined, "harvest/") {
		t.Fatalf("path-like literals must also index their prefix, got %v", keys)
	}
}
