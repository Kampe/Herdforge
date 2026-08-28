package main

import (
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// A root bundle is the authority boundary for a mutating ingest: every default
// path must trace to the same project, even when an inherited lane root is
// stale. This is deliberately table-driven so a newly added path cannot hide
// outside the assertion.
func TestReviewIngestRootBundleUsesOneCanonicalProject(t *testing.T) {
	projectRoot := t.TempDir()
	laneRoot := t.TempDir()
	t.Setenv("HERD_PROJECT_ROOT", projectRoot)
	t.Setenv("HERD_ROOT", laneRoot)
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	t.Setenv("HERD_REVIEW_LEDGER", "")

	roots, err := resolveReviewIngestRoots(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root bundle: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{name: "project", got: roots.ProjectRoot, want: projectRoot},
		{name: "review repository", got: roots.Review.RepoRoot, want: projectRoot},
		{name: "review corpus", got: roots.Review.Paths.Root, want: filepath.Join(projectRoot, ".herd", "review")},
		{name: "receipt", got: roots.ReceiptPath, want: launch.PathFor(projectRoot)},
		{name: "ledger", got: roots.LedgerPath, want: reviewledger.PathFor(projectRoot)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s path = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
	if err := roots.requireMutationSafe(); err != nil {
		t.Fatalf("same-project root bundle must be writable: %v", err)
	}
}
