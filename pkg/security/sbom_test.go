package security

import (
	"context"
	"testing"
)

func TestVerifySBOMCompliance_Empty(t *testing.T) {
	s := NewSecurityScanner()
	err := s.VerifySBOMCompliance(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil SBOM content")
	}

	err = s.VerifySBOMCompliance(context.Background(), []byte{})
	if err == nil {
		t.Fatal("expected error for empty SBOM content")
	}
}

func TestVerifySBOMCompliance_Valid(t *testing.T) {
	s := NewSecurityScanner()
	err := s.VerifySBOMCompliance(context.Background(), []byte(`{"bomFormat":"CycloneDX","version":1}`))
	if err != nil {
		t.Fatalf("expected nil error for valid SBOM, got: %v", err)
	}
}
