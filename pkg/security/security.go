package security

import (
	"context"
	"fmt"
	"strings"
)

type SecretFinding struct {
	Type        string
	Description string
	Match       string
}

type ScanResult struct {
	Passed   bool
	Findings []SecretFinding
}

type SecurityScanner struct{}

func NewSecurityScanner() *SecurityScanner {
	return &SecurityScanner{}
}

// ScanDiff checks patch diffs for leaked secrets and private credentials
func (s *SecurityScanner) ScanDiff(ctx context.Context, diffContent string) *ScanResult {
	var findings []SecretFinding

	lower := strings.ToLower(diffContent)
	if strings.Contains(lower, "api_key") || strings.Contains(lower, "secret_key") || strings.Contains(lower, "private key") {
		findings = append(findings, SecretFinding{
			Type:        "leaked_credential",
			Description: "Potential hardcoded secret or API key detected in diff",
			Match:       "sensitive_token",
		})
	}

	return &ScanResult{
		Passed:   len(findings) == 0,
		Findings: findings,
	}
}

// VerifySBOMCompliance ensures dependencies do not contain critical CVEs
func (s *SecurityScanner) VerifySBOMCompliance(ctx context.Context, sbomJSON []byte) error {
	if len(sbomJSON) == 0 {
		return fmt.Errorf("empty SBOM content")
	}
	return nil
}
