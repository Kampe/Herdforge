package release

import (
	"context"
	"testing"
)

func TestReleaseEngine_GenerateChangelog(t *testing.T) {
	rel := NewReleaseEngine(".")
	notes, markdown, err := rel.GenerateChangelog(context.Background(), "", "v0.1.0")
	if err != nil {
		t.Fatalf("expected clean changelog generation, got err: %v", err)
	}

	if notes.Version != "v0.1.0" {
		t.Errorf("unexpected release version: %s", notes.Version)
	}

	if len(markdown) == 0 {
		t.Errorf("expected non-empty markdown changelog")
	}
}

func TestGenerateChangelog_SpecificFromTag(t *testing.T) {
	rel := NewReleaseEngine(".")
	notes, _, err := rel.GenerateChangelog(context.Background(), "HEAD~5", "v0.3.0")
	if err != nil {
		t.Fatalf("expected clean changelog with fromTag, got err: %v", err)
	}
	if notes.Version != "v0.3.0" {
		t.Errorf("unexpected version: %s", notes.Version)
	}
}

func TestGenerateChangelog_GitError(t *testing.T) {
	rel := NewReleaseEngine("/nonexistent-repo-path-xyzzy")
	_, _, err := rel.GenerateChangelog(context.Background(), "", "v1.0.0")
	if err == nil {
		t.Fatal("expected error for git command in nonexistent dir")
	}
}
