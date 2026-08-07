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

// Same swallowed-flag defect on the approve path the loop drives.
func TestParseApproveArgs_FlagsAfterRef(t *testing.T) {
	ref, evidence, force := parseApproveArgs([]string{"FAC-2", "--force", "--evidence", "deadbeef"})
	if ref != "FAC-2" {
		t.Fatalf("ref=%q want FAC-2", ref)
	}
	if !force {
		t.Fatal("--force after the ref was swallowed")
	}
	if evidence != "deadbeef" {
		t.Fatalf("--evidence after the ref was swallowed: %q", evidence)
	}
	if ref, evidence, force := parseApproveArgs([]string{"FAC-2"}); ref != "FAC-2" || evidence != "" || force {
		t.Fatalf("bare ref must not force or carry evidence: %q %q %v", ref, evidence, force)
	}
}
