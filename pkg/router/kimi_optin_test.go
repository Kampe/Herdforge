package router

import (
	"strings"
	"testing"
)

// TestKimiIsNotAFallbackCandidate is the FAC-573 regression.
//
// A dispatch was observed reaching a Kimi target and failing after leaving an
// orphan tab. There is no Kimi account on this fleet, so selecting it is a
// guaranteed failure: the launch dies or waits at an authentication screen while
// the lane still reports alive. Kimi sat in the qa and adversarial waterfalls,
// so exhausting the real providers would route there.
func TestKimiIsNotAFallbackCandidate(t *testing.T) {
	t.Setenv("HERD_ENABLE_KIMI", "")
	for _, shape := range AllShapes() {
		got, err := Waterfall(shape)
		if err != nil {
			t.Fatalf("%s: %v", shape, err)
		}
		for _, p := range got {
			if strings.EqualFold(p, "kimi") {
				t.Fatalf("shape %q still offers kimi as a candidate: %v", shape, got)
			}
		}
	}
}

// The real providers must survive the filter: this must not become a quieter
// allowlist that drops usable surfaces.
func TestFilterKeepsRealProviders(t *testing.T) {
	t.Setenv("HERD_ENABLE_KIMI", "")
	got, err := Waterfall("adversarial")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"grok", "claude", "agy", "codex"} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("adversarial must still offer %q, got %v", want, got)
		}
	}
	if len(got) == 0 {
		t.Fatal("filter must not empty a waterfall")
	}
}

// An environment that genuinely has an account can opt back in.
func TestOptInRestoresKimi(t *testing.T) {
	t.Setenv("HERD_ENABLE_KIMI", "1")
	got, err := Waterfall("adversarial")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range got {
		if p == "kimi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit opt-in must restore kimi, got %v", got)
	}
}

// The gate must fail closed on anything other than exactly "1".
func TestOptInFailsClosedOnOddValues(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes", " 1 x"} {
		t.Setenv("HERD_ENABLE_KIMI", v)
		got, _ := Waterfall("qa")
		for _, p := range got {
			if p == "kimi" {
				t.Fatalf("value %q must not enable kimi", v)
			}
		}
	}
}
