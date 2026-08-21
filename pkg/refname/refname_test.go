package refname

import (
	"strings"
	"testing"
)

// TestNoMainSubstring covers both generators' real inputs, including the exact
// name a publication guard refused.
func TestNoMainSubstring(t *testing.T) {
	for _, in := range []string{
		"reconstruct/cha-2197-current-main",
		"reconstruct/cha-2195-current-main-refresh",
		"review/cha-2172-current-main-refresh",
		"feature/Main-thing",
		"MAIN",
		"main",
	} {
		got := PublishSafeSegment(in)
		if strings.Contains(strings.ToLower(got), "main") {
			t.Fatalf("segment for %q still contains main: %q", in, got)
		}
		if got == "" {
			t.Fatalf("segment for %q must not be empty", in)
		}
		if strings.ContainsAny(got, "/ ") {
			t.Fatalf("segment %q must be one ref segment", got)
		}
	}
}

// The descriptive portion must stay recognizable: identity is the SHA the
// caller appends, but an operator still needs to know the source.
func TestStaysRecognizable(t *testing.T) {
	got := PublishSafeSegment("reconstruct/cha-2197-current-main")
	for _, want := range []string{"reconstruct", "cha-2197", "trunk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("segment %q must retain %q", got, want)
		}
	}
	// The reported failing name used underscores from a different sanitizer;
	// one definition means one spelling now.
	if strings.Contains(got, "_") {
		t.Fatalf("segment must use a single separator convention, got %q", got)
	}
}

// Sanitizing must not leave separator runs or edge separators.
func TestNoSeparatorRunsOrEdges(t *testing.T) {
	got := PublishSafeSegment("//weird**name//")
	if strings.Contains(got, "--") {
		t.Fatalf("separator run left in %q", got)
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Fatalf("edge separator left in %q", got)
	}
}
