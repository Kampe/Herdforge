package reviewingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the checked-in instructions, not a second hand-written artifact
// that can stay green while the template tells reviewers to omit real bindings.
func TestRepositoryVerdictTemplateIsIngestible(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".herd", "prompts", "review-verdict.template.md"))
	if err != nil {
		t.Fatal(err)
	}
	sha, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	values := map[string]string{
		"sha": sha, "branch": "fix/template", "task": "FAC-762",
		"reviewer": "review-fac-762", "reviewer-family": "google", "builder-family": "openai",
		"verdict": "PASS", "reviewed-base": base, "reviewed-head": sha,
		"retry-of": "prior-review-lane", "reassesses": strings.Repeat("c", 64),
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if line == "---" {
			break
		}
		key, _, ok := strings.Cut(line, ":")
		if value, exists := values[key]; ok && exists {
			lines[i] = key + ": " + value
		}
	}
	// Synthetic fixture observations only: these are not live review evidence.
	evidence := strings.Repeat("The fixture checks exact candidate identity and preserves the independent review boundary. ", 4)
	verification := "- `go test ./pkg/reviewingest` — PASS in this fixture."
	filled := strings.NewReplacer("{{review-evidence}}", evidence, "{{executed-verification}}", verification).Replace(strings.Join(lines, "\n"))
	a := Parse(filled)
	if err := a.Validate(nil, func(s string) bool { return s == sha }); err != nil {
		t.Errorf("filled repository template is refused: %v", err)
	}
	if a.SHA != sha || a.ReadHead != sha || a.ReadBase != base || a.TaskRef != "FAC-762" ||
		a.Reviewer != "review-fac-762" || a.ReviewerFamily != "google" || a.BuilderFamily != "openai" {
		t.Errorf("template lost exact review identity: %+v", a)
	}
	if a.RetryOf != values["retry-of"] || a.Reassesses != values["reassesses"] {
		t.Errorf("optional retry/reassessment bindings not retained: %+v", a)
	}
	if got := a.VerificationEvidence(); got != verification {
		t.Errorf("verification must contain only executed observations, got %q", got)
	}
	if a.VerificationDigest() == "" {
		t.Error("recorded verification has no digest")
	}
	withoutChecks := Parse(strings.Replace(filled, verification, "", 1))
	if got := withoutChecks.VerificationDigest(); got != "" {
		t.Errorf("instructions alone produced verification digest %q", got)
	}
}
