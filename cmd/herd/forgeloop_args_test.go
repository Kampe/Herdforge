package main

import "testing"

// FAC-138: `herd review FAC-1 --spawn` is the form the forge loop emits and the
// form operators type. Go's flag package stops at the first positional, so the
// flag was parsed as false and NO reviewer was ever spawned — the loop's review
// step silently did nothing.
func TestParseReviewArgs_SpawnAfterRef(t *testing.T) {
	for _, args := range [][]string{
		{"FAC-1", "--spawn"},
		{"--spawn", "FAC-1"},
	} {
		ref, spawn := parseReviewArgs(args)
		if ref != "FAC-1" {
			t.Fatalf("%v: ref=%q want FAC-1", args, ref)
		}
		if !spawn {
			t.Fatalf("%v: --spawn was swallowed; no reviewer would start", args)
		}
	}
	if ref, spawn := parseReviewArgs([]string{"FAC-1"}); ref != "FAC-1" || spawn {
		t.Fatalf("bare ref must not spawn: ref=%q spawn=%v", ref, spawn)
	}
}

// Same swallowed-flag defect on the approve path the loop drives. FAC-132
// replaced --force/--evidence with --receipt and the --override-* quartet;
// the property under test is unchanged — a flag placed AFTER the ref must
// still reach the parser.
func TestParseApproveArgs_FlagsAfterRef(t *testing.T) {
	ref, receipt, ov := parseApproveArgs([]string{
		"FAC-2", "--receipt", "r.json",
		"--override-policy", "duplicate-card", "--override-actor", "kampe",
		"--override-reason", "dupe", "--override-evidence", "deadbeef",
	})
	if ref != "FAC-2" {
		t.Fatalf("ref=%q want FAC-2", ref)
	}
	if receipt != "r.json" {
		t.Fatalf("--receipt after the ref was swallowed: %q", receipt)
	}
	req, err := ov.request()
	if err != nil {
		t.Fatalf("override request: %v", err)
	}
	if req == nil {
		t.Fatal("the --override-* quartet after the ref was swallowed entirely")
	}
	if req.Policy != "duplicate-card" || req.Actor != "kampe" || req.Reason != "dupe" || req.Evidence != "deadbeef" {
		t.Fatalf("an --override-* flag after the ref was swallowed: %+v", req)
	}

	ref, receipt, ov = parseApproveArgs([]string{"FAC-2"})
	req, err = ov.request()
	if err != nil {
		t.Fatalf("bare ref: %v", err)
	}
	if ref != "FAC-2" || receipt != "" || req != nil {
		t.Fatalf("bare ref must carry no receipt and no override: %q %q %+v", ref, receipt, req)
	}
}

// --force is refused even when it lands after the ref, so the old muscle
// memory fails loudly instead of being parsed as a swallowed false.
func TestParseApproveArgs_ForceAfterRefIsRefused(t *testing.T) {
	_, _, ov := parseApproveArgs([]string{"FAC-2", "--force"})
	if _, err := ov.request(); err == nil {
		t.Fatal("--force after the ref must be refused, not silently ignored")
	}
}
