package herdr

import "testing"

// TestWorkspaceTrustPromptIsDistinctFromLogin is the FAC-576 gate.
//
// Claude Code asks whether to trust a directory the first time it runs there. A
// fleet reviewer launches into a review surface Herdforge just created, so the
// dialog appears on nearly every exact-SHA review — and the launch check read
// "trust this folder" as an auth screen and refused. The reported symptom was
// "pane is at a login or authentication screen" on a host where claude was fully
// logged in.
//
// Verified live: neither --permission-mode bypassPermissions nor
// --dangerously-skip-permissions suppresses the dialog.
func TestWorkspaceTrustPromptIsDistinctFromLogin(t *testing.T) {
	// Captured from a real pane.
	trust := "Do you trust the files in this folder?\n\n Security guide\n\n" +
		" ❯ 1. Yes, I trust this folder\n   2. No, exit\n\n Enter to confirm · Esc to cancel"
	if !WorkspaceTrustPrompt(trust) {
		t.Fatal("the real trust dialog must be recognized")
	}
	// A genuine login screen must NOT be mistaken for a resolvable dialog: we
	// cannot answer it, and pressing Enter at one would be meaningless.
	for _, login := range []string{
		"Please log in to continue",
		"Authentication required. Visit https://example.com to authenticate",
		"not logged in",
	} {
		if WorkspaceTrustPrompt(login) {
			t.Errorf("a login screen must not be classified as a trust dialog: %q", login)
		}
	}
	// A healthy session is neither.
	healthy := "Claude Code v2.1.220\nSonnet 5 with medium effort · Claude Team\n❯"
	if WorkspaceTrustPrompt(healthy) {
		t.Error("a healthy session is not a trust dialog")
	}
	if LoginOrAuthScreen("✳ Claude Code", healthy) {
		t.Error("a healthy session is not a login screen")
	}
}

// Requiring the numbered options as well as the phrase is what keeps a
// credential screen that merely mentions trust from being auto-confirmed.
func TestTrustDetectionNeedsTheOptions(t *testing.T) {
	if WorkspaceTrustPrompt("we could not verify that you trust this folder; please log in") {
		t.Error("the phrase alone must not be enough to press Enter at a pane")
	}
}
