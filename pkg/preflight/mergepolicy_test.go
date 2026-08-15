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

func TestCheckMergePolicy_RemoteCIIsOptInButRequiresChecksWhenEnabled(t *testing.T) {
	policy := DefaultProtectedPolicy()
	if rep := CheckMergePolicy(policy); !rep.OK {
		t.Fatalf("remote CI opt-out policy refused: %v", rep.Reasons)
	}
	policy.RemoteCI.Required = true
	if rep := CheckMergePolicy(policy); rep.OK {
		t.Fatal("remote CI without declared checks was accepted")
	}
	policy.RemoteCI.RequiredChecks = []string{"remote-gate"}
	if rep := CheckMergePolicy(policy); !rep.OK {
		t.Fatalf("declared remote CI policy refused: %v", rep.Reasons)
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

func TestLoadMergePolicyHonorsRuntimeProfile(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(t.TempDir(), "herd.yaml")
	if err := os.WriteFile(profile, []byte("merge_policy:\n  protected: true\n  required_checks: [profile-gate]\n  require_different_family_review: true\n  require_pull_request_reviews: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CONFIG_PATH", profile)
	t.Chdir(root)
	p, err := LoadMergePolicy(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.RequiredChecks) != 1 || p.RequiredChecks[0] != "profile-gate" {
		t.Fatalf("runtime profile was ignored: %+v", p)
	}
}

func TestLoadMergePolicyRejectsUnmigratedLegacyFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte("version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "merge-policy.yaml"), []byte("required_checks: [legacy-gate]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMergePolicy(root); err == nil || !strings.Contains(err.Error(), "legacy merge policy") {
		t.Fatalf("unmigrated legacy policy was not rejected: %v", err)
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
// runs. This fails if merge_policy is deleted-weakened rather than omitted
// (omission falls back to DefaultProtectedPolicy).
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

func TestPolicyRevisionChangesWhenMergeContractChanges(t *testing.T) {
	base := DefaultProtectedPolicy()
	got := PolicyRevision(base)
	if got == "" || !strings.HasPrefix(got, "merge-policy-v2:") {
		t.Fatalf("revision = %q, want versioned digest", got)
	}
	base.RequiredChecks = append(base.RequiredChecks, "Hermetic")
	if changed := PolicyRevision(base); changed == got {
		t.Fatal("merge policy revision did not change after contract change")
	}
}

func TestPolicyRevisionNormalizesCheckOrderAndWhitespace(t *testing.T) {
	a := DefaultProtectedPolicy()
	a.RequiredChecks = []string{" Gate ", "Build"}
	b := a
	b.RequiredChecks = []string{"Build", "Gate"}
	if PolicyRevision(a) != PolicyRevision(b) {
		t.Fatal("equivalent required check declarations produced different revisions")
	}
}

func writePolicy(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("version: \"1\"\nmerge_policy:\n")
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
