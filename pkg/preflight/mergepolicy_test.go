package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckMergePolicy_MissingRequiredChecksRefuses(t *testing.T) {
	rep := CheckMergePolicy(MergePolicy{
		Protected:                    true,
		RequiredChecks:               nil,
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	})
	if rep.OK {
		t.Fatal("empty required_checks was accepted for a protected repo")
	}
	if !strings.Contains(strings.Join(rep.Reasons, " "), "required_checks") {
		t.Fatalf("reasons = %v", rep.Reasons)
	}
}

func TestCheckMergePolicy_NoDifferentFamilyRefuses(t *testing.T) {
	rep := CheckMergePolicy(MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"gate"},
		RequireDifferentFamilyReview: false,
		RequirePullRequestReviews:    true,
	})
	if rep.OK {
		t.Fatal("missing different-family review was accepted")
	}
}

func TestCheckMergePolicy_NoPullRequestReviewsRefuses(t *testing.T) {
	rep := CheckMergePolicy(MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"gate"},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    false,
	})
	if rep.OK {
		t.Fatal("missing require_pull_request_reviews was accepted")
	}
}

func TestCheckMergePolicy_CompletePolicyPasses(t *testing.T) {
	rep := CheckMergePolicy(DefaultProtectedPolicy())
	if !rep.OK {
		t.Fatalf("complete policy failed: %v", rep.Reasons)
	}
}

// The single edit that would neutralise every other gate. If `protected: false`
// ever returns OK again, the whole check becomes unable to refuse anything.
func TestCheckMergePolicy_UnprotectedIsRefusedNotOpen(t *testing.T) {
	rep := CheckMergePolicy(MergePolicy{
		Protected:                    false,
		RequiredChecks:               []string{"gate"},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	})
	if rep.OK {
		t.Fatal("protected: false opened the gate")
	}
	if !strings.Contains(strings.Join(rep.Reasons, " "), "protected: false") {
		t.Fatalf("reasons = %v", rep.Reasons)
	}
}

func TestLoadMergePolicy_MissingDefaultsProtected(t *testing.T) {
	root := t.TempDir()
	p, err := LoadMergePolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Protected || !p.RequireDifferentFamilyReview || len(p.RequiredChecks) == 0 {
		t.Fatalf("missing file must default protected: %+v", p)
	}
}

func TestLoadMergePolicy_ParsesFile(t *testing.T) {
	root := t.TempDir()
	writePolicy(t, root, "protected: true\nrequired_checks:\n  - gate-a\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\n")
	p, err := LoadMergePolicy(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.RequiredChecks) != 1 || p.RequiredChecks[0] != "gate-a" {
		t.Fatalf("policy = %+v", p)
	}
}

func TestLoadMergePolicy_MalformedYAMLIsAnError(t *testing.T) {
	root := t.TempDir()
	writePolicy(t, root, "protected: [not-a-bool\n")
	if _, err := LoadMergePolicy(root); err == nil {
		t.Fatal("malformed policy file parsed without error")
	}
}

// The gate's entire scope is the DECLARATION, so the thing to prove is that it
// discriminates: every way of weakening the declaration is refused, and only the
// complete one passes. Same root, same call — if any weakening ever returns nil,
// the gate has stopped discriminating.
func TestRefuseAutonomousMerge_EveryWeakenedDeclarationIsBlocked(t *testing.T) {
	const complete = "protected: true\nrequired_checks:\n  - gate-a\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\nremote_ci:\n  required: true\n  required_checks:\n    - gate-a\n"
	weakened := map[string]string{
		"unprotected":        "protected: false\nrequired_checks:\n  - gate-a\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\n",
		"no required checks": "protected: true\nrequired_checks: []\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\n",
		"no family review":   "protected: true\nrequired_checks:\n  - gate-a\nrequire_different_family_review: false\nrequire_pull_request_reviews: true\n",
		"no pr reviews":      "protected: true\nrequired_checks:\n  - gate-a\nrequire_different_family_review: true\nrequire_pull_request_reviews: false\n",
		"blank check names":  "protected: true\nrequired_checks:\n  - \"  \"\nrequire_different_family_review: true\nrequire_pull_request_reviews: true\n",
	}
	root := t.TempDir()
	for name, body := range weakened {
		writePolicy(t, root, body)
		err := RefuseAutonomousMerge(root)
		if err == nil {
			t.Fatalf("%s: weakened declaration was allowed", name)
		}
		if !strings.Contains(err.Error(), "BLOCKED(merge_policy)") {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	writePolicy(t, root, complete)
	if err := RefuseAutonomousMerge(root); err != nil {
		t.Fatalf("complete declaration refused: %v", err)
	}
}

// The repository's own committed policy must satisfy the gate `make preflight`
// runs. This fails if .herd/merge-policy.yaml is deleted-weakened rather than
// deleted outright (deletion falls back to DefaultProtectedPolicy).
func TestRefuseAutonomousMerge_ThisRepositoryPasses(t *testing.T) {
	if err := RefuseAutonomousMerge("../.."); err != nil {
		t.Fatalf("repository merge policy is not admissible: %v", err)
	}
}

// Mutation probe: if someone deletes the "empty required_checks" guard, this
// fails. A vacuous test would assert OK on the empty case.
func TestCheckMergePolicy_EmptyChecksIsNotOK(t *testing.T) {
	rep := CheckMergePolicy(MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"", "  "},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	})
	if rep.OK {
		t.Fatal("whitespace-only required_checks must not pass")
	}
}

func writePolicy(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "merge-policy.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
