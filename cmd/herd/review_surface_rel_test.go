package main

import (
	"path/filepath"
	"testing"
)

// FAC-588: the review surface symlink target is computed with filepath.Rel,
// which REFUSES to mix an absolute base with a relative one. The pool lease path
// is always absolute; --surface-root defaults to the relative
// ".herd/review-surfaces". So the default invocation died after taking a pool
// lease and before launching a reviewer, and a caller that lost stderr saw
// `herd review --pool` do nothing at all. That is why the reviewer pool looked
// permanently unfillable.
//
// This asserts the mixed-ness is the failure mode and that absolutising both
// sides fixes it — the exact transformation the fix applies.
func TestReviewSurfaceRelRequiresBothSidesAbsolute(t *testing.T) {
	absLease := "/repo/.herd/pool/pool-02"
	relSurfaceDir := ".herd/review-surfaces"

	// The pre-fix computation: mixed, and it must fail.
	if _, err := filepath.Rel(relSurfaceDir, absLease); err == nil {
		t.Fatal("filepath.Rel is expected to refuse a relative base with an absolute target; " +
			"if it stopped refusing, this fix is no longer load-bearing and the comment is stale")
	}

	// The post-fix computation: both absolute, and it must succeed and resolve
	// back to the lease.
	surfaceDirAbs, err := filepath.Abs(relSurfaceDir)
	if err != nil {
		t.Fatalf("abs surface dir: %v", err)
	}
	leaseAbs, err := filepath.Abs(absLease)
	if err != nil {
		t.Fatalf("abs lease: %v", err)
	}
	rel, err := filepath.Rel(surfaceDirAbs, leaseAbs)
	if err != nil {
		t.Fatalf("absolutised Rel must succeed, got %v", err)
	}
	// The symlink is created inside surfaceDirAbs, so joining must land on the
	// lease. A relative target that does not resolve back is a broken surface.
	if got := filepath.Clean(filepath.Join(surfaceDirAbs, rel)); got != filepath.Clean(leaseAbs) {
		t.Errorf("symlink target does not resolve to the lease: %q -> %q, want %q",
			rel, got, filepath.Clean(leaseAbs))
	}
}

// An already-absolute --surface-root was the documented workaround, so it must
// keep working.
func TestReviewSurfaceRelWorksWhenSurfaceRootAlreadyAbsolute(t *testing.T) {
	surfaceDir := "/repo/.herd/review-surfaces"
	lease := "/repo/.herd/pool/pool-03"
	rel, err := filepath.Rel(surfaceDir, lease)
	if err != nil {
		t.Fatalf("both-absolute Rel must succeed: %v", err)
	}
	if got := filepath.Clean(filepath.Join(surfaceDir, rel)); got != lease {
		t.Errorf("resolved to %q want %q", got, lease)
	}
}
