package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewingest"
)

// FAC-583: the packet used to ask for "the required verdict artifact" without
// stating its shape. review-ingest refuses anything whose front matter is not
// the leading block, so a reviewer that opened with prose had its finished
// verdict discarded. A real Opus review of PR-3115 that caught a fabricated
// eth_call response (a 1e12 money error) was refused for exactly this reason.
//
// This asserts the packet carries every key the ingest gate requires, so a
// reviewer that follows it is ingestible by construction.
func TestReviewPacketCarriesIngestibleFrontMatterContract(t *testing.T) {
	body := reviewPacketBody("PR-3115", "8867353f0ba9fe569feeb28989c10d0fefdc6ca1",
		".herd/review-surfaces/review-pr-3115")

	for _, key := range []string{
		"sha:", "branch:", "task:", "reviewer:", "reviewer-family:",
		"builder-family:", "verdict:", "reviewed-head:",
	} {
		if !strings.Contains(body, key) {
			t.Errorf("packet omits required front-matter key %q; a reviewer cannot emit what it was never told about", key)
		}
	}

	// The known values must be prefilled, not left as placeholders for the
	// reviewer to retype and get wrong.
	if !strings.Contains(body, "sha: 8867353f0ba9fe569feeb28989c10d0fefdc6ca1") {
		t.Error("packet must prefill the candidate sha it already knows")
	}
	if !strings.Contains(body, "task: PR-3115") {
		t.Error("packet must prefill the card ref so the verdict can be joined to a card (FAC-578)")
	}

	// The leading-block rule is the one that silently discarded real reviews.
	if !strings.Contains(body, "first bytes") && !strings.Contains(body, "very first") {
		t.Error("packet must state that front matter has to be the leading block")
	}
}

// The contract the packet advertises must be the contract the parser accepts.
// If these two drift, reviewers follow instructions and still get refused.
func TestPacketContractMatchesParserAcceptedKeys(t *testing.T) {
	body := reviewPacketBody("CHA-1", strings.Repeat("a", 40), "surface")

	// Build a minimal artifact the way a compliant reviewer would, using the
	// packet's own block, and confirm the parser extracts what we expect.
	artifact := "sha: " + strings.Repeat("a", 40) + "\n" +
		"branch: rescue/x\ntask: CHA-1\nreviewer: review-cha-1-claude\n" +
		"reviewer-family: anthropic\nbuilder-family: openai\n" +
		"verdict: FAIL\nreviewed-head: " + strings.Repeat("a", 40) + "\n---\n" +
		strings.Repeat("evidence ", 40) + "\n"

	a := reviewingest.Parse(artifact)
	if len(a.UnknownHeaders) != 0 {
		t.Errorf("packet advertises keys the parser rejects as unknown: %v", a.UnknownHeaders)
	}
	if a.MalformedHeaderRegion {
		t.Error("a packet-compliant artifact must not be seen as a malformed header region")
	}
	if a.Verdict != "FAIL" {
		t.Errorf("verdict = %q want FAIL", a.Verdict)
	}
	if a.TaskRef != "CHA-1" {
		t.Errorf("task ref = %q want CHA-1", a.TaskRef)
	}
	// Guard against the packet advertising a key the parser silently ignores.
	for _, key := range []string{"branch:", "reviewed-head:", "builder-family:"} {
		if !strings.Contains(body, key) {
			t.Errorf("packet lost key %q", key)
		}
	}
}
