package main

import (
	"bytes"
	"strings"
	"testing"

	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// A closed ticket must never keep holding scope, on ANY successful path.
// FAC-132 added an exactly-once short-circuit that skips the board write on
// redelivery; an early return there would also skip the scope release, and
// re-running `herd board-done` is the documented recovery for a crash between
// the recorded close and the release. That recovery must not become a
// permanent no-op (the FAC-174/FAC-198 stuck-scope class).
func TestFinishBoardDoneAlwaysReleasesScope(t *testing.T) {
	cases := []struct {
		name         string
		res          *hsync.DoneResult
		wantInOut    string
		wantNotInOut string
	}{
		{
			name:      "first close",
			res:       &hsync.DoneResult{Ref: "FAC-132", Proof: "completion receipt abc", ReceiptDigest: "abc"},
			wantInOut: "verified by read-back",
		},
		{
			name:         "idempotent redelivery",
			res:          &hsync.DoneResult{Ref: "FAC-132", Proof: "completion receipt abc", ReceiptDigest: "abc", Idempotent: true},
			wantInOut:    "no board change this run",
			wantNotInOut: "verified by read-back",
		},
		{
			name:      "manual override close",
			res:       &hsync.DoneResult{Ref: "FAC-132", Proof: "manual override by kampe", Overridden: true},
			wantInOut: "verified by read-back",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var released []string
			var out bytes.Buffer
			finishBoardDone(&out, tc.res, func(ref string) { released = append(released, ref) })

			if len(released) != 1 || released[0] != tc.res.Ref {
				t.Fatalf("scope claim must be released exactly once for %s, got %v", tc.res.Ref, released)
			}
			if !strings.Contains(out.String(), tc.wantInOut) {
				t.Fatalf("output %q does not mention %q", out.String(), tc.wantInOut)
			}
			if tc.wantNotInOut != "" && strings.Contains(out.String(), tc.wantNotInOut) {
				t.Fatalf("a run that wrote nothing must not claim %q: %q", tc.wantNotInOut, out.String())
			}
		})
	}
}

// The manual-override quartet is all-or-nothing, and --force is refused
// outright: it closed cards without recording who forced what.
func TestOverrideFlagsRequest(t *testing.T) {
	build := func(policy, actor, reason, evidence string, force bool) overrideFlags {
		return overrideFlags{policy: &policy, actor: &actor, reason: &reason, evidence: &evidence, force: &force}
	}

	t.Run("no override flags yields no override", func(t *testing.T) {
		req, err := build("", "", "", "", false).request()
		if err != nil || req != nil {
			t.Fatalf("bare invocation must carry no override, got %+v err %v", req, err)
		}
	})

	t.Run("force is refused with the replacement named", func(t *testing.T) {
		_, err := build("", "", "", "", true).request()
		if err == nil || !strings.Contains(err.Error(), "--override-policy") {
			t.Fatalf("--force must be refused and name its replacement, got %v", err)
		}
	})

	t.Run("a partial quartet still reaches the policy gate", func(t *testing.T) {
		// The CLI assembles it; pkg/sync refuses it. What must never happen is
		// a partial override silently degrading into "no authority at all",
		// which would fall through to the no-receipt refusal instead.
		req, err := build("operator-external-merge", "", "", "", false).request()
		if err != nil {
			t.Fatalf("unexpected CLI error: %v", err)
		}
		if req == nil {
			t.Fatal("a partial override must NOT vanish into a nil request")
		}
	})

	t.Run("a complete quartet is passed through verbatim", func(t *testing.T) {
		req, err := build("duplicate-card", "kampe", "dupe of FAC-1", "sha123", false).request()
		if err != nil || req == nil {
			t.Fatalf("complete quartet must build, got %+v err %v", req, err)
		}
		if req.Policy != "duplicate-card" || req.Actor != "kampe" || req.Reason != "dupe of FAC-1" || req.Evidence != "sha123" {
			t.Fatalf("override request lost a field: %+v", req)
		}
	})
}

// Every policy the CLI advertises must be one pkg/sync actually permits, or
// the help text sends operators at a refusal.
func TestOverridePolicyListMatchesPolicySet(t *testing.T) {
	listed := overridePolicyList()
	if strings.TrimSpace(listed) == "" {
		t.Fatal("the CLI must advertise the permitted policies")
	}
	for _, p := range hsync.SortedOverridePolicies() {
		if !strings.Contains(listed, p) {
			t.Fatalf("policy %q is permitted but not advertised in %q", p, listed)
		}
	}
	for _, p := range strings.Split(listed, ", ") {
		if _, ok := hsync.OverridePolicies[strings.TrimSpace(p)]; !ok {
			t.Fatalf("policy %q is advertised but not permitted", p)
		}
	}
}
