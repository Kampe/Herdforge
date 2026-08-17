package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
