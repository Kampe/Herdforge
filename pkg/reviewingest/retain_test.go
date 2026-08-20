package reviewingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainArtifactIsDurableAndIdempotent(t *testing.T) {
	root := t.TempDir()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "ephemeral-verdict.md")
	body := "sha: " + strings.Repeat("a", 40) + "\nverdict: PASS\n---\n" + strings.Repeat("evidence ", 40)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	rel, err := RetainArtifact(root, src, sha, "gemini-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !IsInboxPath(rel) {
		t.Fatalf("retained path %q is not under the review inbox", rel)
	}
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("durable artifact missing: %v", err)
	}
	// Ephemeral source may vanish after retain (reviewer pane cleanup).
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("inbox artifact must survive source cleanup: %v", err)
	}
	retry, err := RetainArtifact(root, dst, sha, "gemini-reviewer")
	if err != nil || retry != rel {
		t.Fatalf("idempotent retain path=%q err=%v want %q", retry, err, rel)
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(InboxRel)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
}

func TestRetainArtifactRefusesVanishedSource(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "gone.md")
	if _, err := RetainArtifact(root, src, strings.Repeat("b", 40), "claude"); err == nil {
		t.Fatal("vanished source must fail closed")
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".herd", "review", "inbox")); err == nil && len(entries) != 0 {
		t.Fatalf("failed retain must not publish inbox entries: %v", entries)
	}
}

func TestIsInboxPath(t *testing.T) {
	if !IsInboxPath(".herd/review/inbox/abc.md") {
		t.Fatal("expected inbox path")
	}
	if IsInboxPath("/var/folders/xx/chainseer-herd-review/abc.md") {
		t.Fatal("temp chainseer path must not count as durable inbox")
	}
}

func TestReviewerQualifiedIngestedNamesAllowSameSHA(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	first := filepath.Join(root, "first.md")
	second := filepath.Join(root, "second.md")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstName := RetainedArtifactName(sha, "reviewer-one", []byte("first"))
	secondName := RetainedArtifactName(sha, "reviewer-two", []byte("second"))
	if firstName == secondName {
		t.Fatalf("reviewer-qualified names collided: %q", firstName)
	}
	if _, err := MoveToIngestedNamed(first, firstName); err != nil {
		t.Fatal(err)
	}
	if _, err := MoveToIngestedNamed(second, secondName); err != nil {
		t.Fatal(err)
	}
}
