package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parityPolicyMessage(source string, optional bool) (string, bool) {
	if source != "" {
		return "run", true
	}
	if optional {
		return "bin-parity: SKIP — optional Chainseer source unavailable (CI-only opt-in)", false
	}
	return "bin-parity: FAIL — Chainseer source unavailable (set CHAINSEER_BIN for an authorized source)", false
}

func TestParityPolicy(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		optional bool
		wantRun  bool
		wantText string
	}{
		{name: "available source remains strict", source: "chainseer/bin", wantRun: true, wantText: "run"},
		{name: "unavailable source skips only when optional", optional: true, wantText: "SKIP"},
		{name: "unavailable source fails by default", wantText: "FAIL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, run := parityPolicyMessage(tc.source, tc.optional)
			if run != tc.wantRun {
				t.Fatalf("run=%v, want %v", run, tc.wantRun)
			}
			if !strings.Contains(got, tc.wantText) {
				t.Fatalf("message=%q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestValidateManifestDispositionRequirements(t *testing.T) {
	base := Manifest{Version: 1, Task: "FAC-309", SourceRoot: "chainseer/bin", SourceExecutableCount: 1, Entries: []Disposition{{Path: "bin/x", Disposition: "herdforge_command_replacement", Replacement: "herd status", Ticket: "FAC-304", Rationale: "ported control-plane command"}}}
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr bool
	}{
		{"valid", func(*Manifest) {}, false},
		{"missing replacement", func(m *Manifest) { m.Entries[0].Replacement = "" }, true},
		{"missing generic ticket", func(m *Manifest) {
			m.Entries[0].Disposition = "generic_capability"
			m.Entries[0].Replacement = ""
			m.Entries[0].Ticket = ""
		}, true},
		{"absolute path", func(m *Manifest) { m.Entries[0].Path = "/bin/x" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mutate(&m)
			if (validateManifest(m) != nil) != tc.wantErr {
				t.Fatalf("validate error mismatch")
			}
		})
	}
}

func TestAuditSourceDetectsMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Task: "FAC-309", SourceRoot: "chainseer/bin", SourceExecutableCount: 0}
	if err := auditSource(dir, m); err == nil {
		t.Fatal("audit accepted an unmanifested executable")
	}
}
