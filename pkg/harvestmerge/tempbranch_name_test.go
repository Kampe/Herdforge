package harvestmerge

import (
	"strings"
	"testing"
)

// TestTempBranchNameAvoidsMainSubstring is the FAC-574 regression at the call
// site a consumer actually invoked.
//
// This generator produced harvest/reconstruct_cha-2197-current-main-17ccbb16ecc0
// and a publication guard matching "main" refused it. FAC-571 had fixed a
// DIFFERENT generator (pkg/resetsafe) with different sanitizing, so the defect
// survived its own fix. The expected value below is byte-identical to the name
// the consumer renamed to by hand, which is the check that the two agree.
func TestTempBranchNameAvoidsMainSubstring(t *testing.T) {
	got := TempBranchName("reconstruct/cha-2197-current-main", "17ccbb16ecc041854bab817213764640aeba1c45")
	const want = "harvest/reconstruct-cha-2197-current-trunk-17ccbb16ecc0"
	if got != want {
		t.Fatalf("TempBranchName = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), "main") {
		t.Fatalf("generated name still contains main: %q", got)
	}
}

// The exact SHA suffix is identity and must be preserved verbatim at 12 chars.
func TestTempBranchNamePreservesShaIdentity(t *testing.T) {
	got := TempBranchName("any/lane", "17ccbb16ecc041854bab817213764640aeba1c45")
	if !strings.HasSuffix(got, "-17ccbb16ecc0") {
		t.Fatalf("SHA identity must be preserved as the suffix, got %q", got)
	}
}
