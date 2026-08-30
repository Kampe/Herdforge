package main

import (
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewroot"
)

func TestParseReviewIngestArgs(t *testing.T) {
	// FAC-572: the default review root is no longer cwd-relative. It comes from
	// the one canonical resolver, so this command cannot audit a different
	// corpus than the queue refers to depending on where it was run.
	defaultRoot := reviewroot.Resolve(".").Root
	defaultLedger := filepath.Join(".herd", "review-ledger.jsonl")
	tests := []struct {
		name          string
		args          []string
		wantFiles     []string
		wantDry       bool
		wantAudit     bool
		wantReresolve bool
		wantErr       bool
	}{
		{name: "artifact then dry run", args: []string{"review.md", "--dry-run"}, wantFiles: []string{"review.md"}, wantDry: true},
		{name: "dry run then artifact", args: []string{"--dry-run", "review.md"}, wantFiles: []string{"review.md"}, wantDry: true},
		{name: "historical provenance re-resolution", args: []string{"review.md", "--reresolve-provenance"}, wantFiles: []string{"review.md"}, wantReresolve: true},
		{name: "flags between artifacts", args: []string{"one.md", "--dry-run", "two.md"}, wantFiles: []string{"one.md", "two.md"}, wantDry: true},
		{name: "audit", args: []string{"--audit", "--audit-root", "review"}, wantAudit: true},
		{name: "unknown flag fails before side effects", args: []string{"review.md", "--dry-run", "--typo"}, wantErr: true},
		{name: "missing flag value fails", args: []string{"review.md", "--ledger"}, wantErr: true},
		{name: "audit artifact ambiguity fails", args: []string{"review.md", "--audit"}, wantErr: true},
		{name: "audit dry run ambiguity fails", args: []string{"--audit", "--dry-run"}, wantErr: true},
		{name: "audit re-resolution ambiguity fails", args: []string{"--audit", "--reresolve-provenance"}, wantErr: true},
		{name: "sweep cannot find historical duplicates", args: []string{"--sweep", "--reresolve-provenance"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReviewIngestArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected parse error for %v, got %+v", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReviewIngestArgs(%v): %v", tc.args, err)
			}
			if got.dryRun != tc.wantDry || got.audit != tc.wantAudit {
				t.Fatalf("flags = dry-run:%v audit:%v, want dry-run:%v audit:%v", got.dryRun, got.audit, tc.wantDry, tc.wantAudit)
			}
			if got.reresolveProvenance != tc.wantReresolve {
				t.Fatalf("reresolve-provenance=%v, want %v", got.reresolveProvenance, tc.wantReresolve)
			}
			if len(got.files) != len(tc.wantFiles) {
				t.Fatalf("files = %v, want %v", got.files, tc.wantFiles)
			}
			for i := range tc.wantFiles {
				if got.files[i] != tc.wantFiles[i] {
					t.Errorf("files[%d] = %q, want %q", i, got.files[i], tc.wantFiles[i])
				}
			}
			if tc.name != "audit" && (got.auditRoot != defaultRoot || got.ledgerPath != defaultLedger) {
				t.Fatalf("defaults = root:%q ledger:%q", got.auditRoot, got.ledgerPath)
			}
		})
	}
}
