package transfer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deterministic interleaving reproduction for the proof-then-unlink race the
// OpenAI evidence review identified (bundle-interleaving-repair-2306): a
// writer replaces the reviewed directory entry in the interval between the
// final identity revalidation and the name-based unlink. Production leaves
// interleaveHook nil, so the seam is inert outside tests.

func TestReclaimRestoresReplacementInstalledAtTheProofUnlinkBoundary(t *testing.T) {
	f := reclaimFixtureSetup(t)
	replacement := filepath.Join(f.bundleDir, "replacement-staging")
	if err := os.WriteFile(replacement, []byte("REPLACEMENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := interleaveHook
	defer func() { interleaveHook = saved }()
	interleaveHook = func(stage string) {
		if stage != "reclaim-pre-unlink" {
			return
		}
		// The reviewed inode is swapped out for a replacement in the race
		// window; the unlink that follows must not delete the replacement.
		if err := os.Rename(replacement, f.bundlePath); err != nil {
			t.Errorf("replacement install: %v", err)
		}
	}
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	body, readErr := os.ReadFile(f.bundlePath)
	if readErr != nil || strings.TrimSpace(string(body)) != "REPLACEMENT" {
		t.Fatalf("replacement directory entry was unlinked instead of the reviewed object: readErr=%v body=%q report=%+v", readErr, body, report)
	}
	if report.Reclaimed != 0 {
		t.Fatalf("a replacement observed at the deletion boundary must not be reported as reclaimed: %+v", report)
	}
}
