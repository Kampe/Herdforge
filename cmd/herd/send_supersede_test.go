package main

import (
	"strings"
	"testing"
)

// TestValidateSupersedeInvocation pins the explicit opt-in contract: the
// replacement payload is mandatory and a drain can never be combined with it.
func TestValidateSupersedeInvocation(t *testing.T) {
	for name, tc := range map[string]struct {
		drain bool
		text  string
		want  string
	}{
		"drain refused":       {drain: true, text: "payload", want: "mutually exclusive"},
		"empty payload":       {text: "   ", want: "requires a replacement payload"},
		"valid":               {text: "retask: new payload", want: ""},
		"file payload counts": {text: "x", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSupersedeInvocation(tc.drain, tc.text)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid invocation refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
