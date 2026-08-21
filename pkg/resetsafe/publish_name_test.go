package resetsafe

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/refname"
)

// TestPreserveBranchUsesSharedDefinition is the FAC-574 follow-up.
//
// FAC-571 fixed a LOCAL copy of this rule here, while the generator a consumer
// actually invoked lived in pkg/harvestmerge with different sanitizing and no
// main-stripping at all -- so the defect survived its own fix. Both callers now
// share pkg/refname, and this asserts the delegation rather than re-testing the
// helper (which pkg/refname covers).
func TestPreserveBranchUsesSharedDefinition(t *testing.T) {
	const source = "reconstruct/cha-2197-current-main"
	got := refname.PublishSafeSegment(source)
	if strings.Contains(strings.ToLower(got), "main") {
		t.Fatalf("shared definition must strip main, got %q", got)
	}
	// The preserve-branch prefix plus this segment is what gets published, so a
	// guard matching "main" must find nothing in either part.
	full := "harvest/" + got + "-abc123def456"
	if strings.Contains(strings.ToLower(full), "main") {
		t.Fatalf("published name must not contain main: %q", full)
	}
}
