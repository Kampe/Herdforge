package resetsafe

import (
	"strings"
	"testing"
)

// TestPublishSafeSegmentAvoidsMainSubstring is the FAC-571 regression.
//
// A generated harvest branch inherited "current-main" from its source branch,
// and a push-safety guard matching "main" anywhere in a git push command refused
// it as a direct-main push. Renaming the identical branch made the push pass, so
// the guard was a false positive produced by our own generated name.
func TestPublishSafeSegmentAvoidsMainSubstring(t *testing.T) {
	cases := []string{
		"reconstruct/cha-2195-current-main",
		"review/cha-2172-current-main-refresh",
		"feature/Main-thing",
		"main",
	}
	for _, in := range cases {
		got := publishSafeSegment(in)
		if strings.Contains(strings.ToLower(got), "main") {
			t.Fatalf("generated segment for %q still contains main: %q", in, got)
		}
		if strings.Contains(got, "/") {
			t.Fatalf("generated segment must be one path segment, got %q", got)
		}
		if got == "" {
			t.Fatalf("segment for %q must not be empty", in)
		}
	}
}

// The descriptive portion must stay recognizable: identity is the appended SHA,
// but an operator still has to know which branch a harvest came from.
func TestPublishSafeSegmentStaysRecognizable(t *testing.T) {
	got := publishSafeSegment("reconstruct/cha-2195-current-main")
	for _, want := range []string{"reconstruct", "cha-2195", "trunk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("segment %q must retain %q", got, want)
		}
	}
}
