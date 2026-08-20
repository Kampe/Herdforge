package sync

import (
	"errors"
	"strings"
	"testing"
)

func TestAcceptanceEvidenceGate(t *testing.T) {
	description := "## Verification\nignored prose\n\n```herd-acceptance-v1\n{\"commands\":[{\"command\":\"go test ./...\",\"context\":\"consumer repo\",\"working_directory\":\"consumer-repo\"}]}\n```"
	tests := []struct {
		name     string
		evidence string
		want     string
	}{
		{"missing output", "", "no pasted output"},
		{"unrelated command", "context: consumer repo\n$ go test ./pkg/...\nPASS\nconsumer-repo", "does not contain acceptance command"},
		{"wrong context", "$ go test ./...\nPASS\nconsumer-repo", "does not identify context"},
		{"wrong directory", "context: consumer repo\n$ go test ./...\nPASS", "does not identify working directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAcceptanceEvidence(description, tt.evidence)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateAcceptanceEvidence error = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, ErrAcceptance) {
				t.Fatalf("error = %v, want ErrAcceptance", err)
			}
		})
	}

	good := "context: consumer repo\nworking directory: consumer-repo\n$ go test ./...\nPASS"
	if err := ValidateAcceptanceEvidence(description, good); err != nil {
		t.Fatalf("genuine pasted acceptance output refused: %v", err)
	}
}

func TestAcceptanceBlockRequired(t *testing.T) {
	_, err := ParseAcceptanceBlock("## Verification\n- [ ] go test ./...")
	if err == nil || !errors.Is(err, ErrAcceptance) {
		t.Fatalf("missing machine-readable block error = %v", err)
	}
}
