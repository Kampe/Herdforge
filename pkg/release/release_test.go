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
