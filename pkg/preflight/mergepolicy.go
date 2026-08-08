// Package preflight — merge-policy declaration lint for protected repositories
// (FAC-135).
//
// A repository that claims to be protected must DECLARE required CI status
// checks and different-family review. This is fail-closed on absence: a missing
// policy file is not "open", it is the protected default. It is a lint over the
// declaration, not per-candidate merge admission — that is reviewledger.Admit.
package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MergePolicy is the repository's declared merge admission contract.
// Loaded from .herd/merge-policy.yaml when present.
type MergePolicy struct {
	// Protected must be true for autonomous merge. Absence of the policy file
	// defaults it true (LoadMergePolicy), and an explicit false is a refusal,
	// not an opt-out — see CheckMergePolicy.
	Protected bool `yaml:"protected"`

	// RequiredChecks are CI job/check names that must be present and green
	// before autonomous merge. Empty is invalid for a protected repository.
	RequiredChecks []string `yaml:"required_checks"`

	// RequireDifferentFamilyReview requires R1–R3 evidence from a model
	// family different from the author before autonomous merge.
	RequireDifferentFamilyReview bool `yaml:"require_different_family_review"`

	// RequirePullRequestReviews is the branch-protection analogue: a human or
	// agent review verdict must exist. Defaults true for protected repos.
	RequirePullRequestReviews bool `yaml:"require_pull_request_reviews"`
}

// DefaultProtectedPolicy is the fail-closed baseline when no policy file exists.
func DefaultProtectedPolicy() MergePolicy {
	return MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"Build, Preflight & Test Suite"},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	}
}

// LoadMergePolicy reads .herd/merge-policy.yaml under root. Missing file yields
// DefaultProtectedPolicy so absence cannot silently open autonomous merge.
func LoadMergePolicy(root string) (MergePolicy, error) {
	path := filepath.Join(root, ".herd", "merge-policy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultProtectedPolicy(), nil
		}
		return MergePolicy{}, fmt.Errorf("read merge policy: %w", err)
	}
	var p MergePolicy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return MergePolicy{}, fmt.Errorf("parse merge policy %s: %w", path, err)
	}
	return p, nil
}

// MergePolicyReport is the structured result of CheckMergePolicy.
type MergePolicyReport struct {
	Policy  MergePolicy
	OK      bool
	Reasons []string
}

// CheckMergePolicy validates the repository's DECLARED merge contract: a
// protected repository that declares no required CI checks, or opts out of
// different-family review or pull-request reviews, fails.
//
// This is deliberately not per-candidate admission. It cannot tell you whether
// CI was green on a branch or whether a different-family reviewer actually
// signed off — that is reviewledger.Admit, which reasons over exact-SHA verdict
// records. Treat this as a config lint that keeps a repo from declaring its way
// out of the gates Admit enforces.
func CheckMergePolicy(policy MergePolicy) MergePolicyReport {
	rep := MergePolicyReport{Policy: policy, OK: true}
	// `protected: false` is not an escape hatch. Without this, any repository
	// could satisfy every gate below by declaring itself unprotected — the one
	// edit a careless or hostile change would make first, which would leave the
	// whole check unable to refuse anything.
	if !policy.Protected {
		rep.OK = false
		rep.Reasons = append(rep.Reasons, "repository declares protected: false; autonomous merge requires a protected repository")
		return rep
	}

	checks := normalizeNames(policy.RequiredChecks)
	if len(checks) == 0 {
		rep.OK = false
		rep.Reasons = append(rep.Reasons, "protected repository declares no required_checks")
	}
	if !policy.RequireDifferentFamilyReview {
		rep.OK = false
		rep.Reasons = append(rep.Reasons, "protected repository must require_different_family_review")
	}
	if !policy.RequirePullRequestReviews {
		rep.OK = false
		rep.Reasons = append(rep.Reasons, "protected repository must require_pull_request_reviews")
	}

	if !rep.OK && len(rep.Reasons) == 0 {
		rep.Reasons = append(rep.Reasons, "merge policy validation failed")
	}
	return rep
}

// RefuseAutonomousMerge returns a non-nil error when CheckMergePolicy fails.
// Callers must treat this as hard BLOCKED. Scope is the declared policy only —
// see CheckMergePolicy.
func RefuseAutonomousMerge(root string) error {
	policy, err := LoadMergePolicy(root)
	if err != nil {
		return fmt.Errorf("merge policy: %w", err)
	}
	rep := CheckMergePolicy(policy)
	if rep.OK {
		return nil
	}
	return fmt.Errorf("BLOCKED(merge_policy): autonomous merge refused: %s", strings.Join(rep.Reasons, "; "))
}

func normalizeNames(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func containsName(hay []string, needle string) bool {
	n := strings.ToLower(strings.TrimSpace(needle))
	for _, h := range hay {
		if strings.ToLower(h) == n {
			return true
		}
	}
	return false
}
