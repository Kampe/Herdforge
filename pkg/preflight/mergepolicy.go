// Package preflight — merge-policy declaration lint for protected repositories
// (FAC-135).
//
// A repository that claims to be protected must DECLARE required CI status
// checks and different-family review. This is fail-closed on absence: a missing
// policy file is not "open", it is the protected default. It is a lint over the
// declaration, not per-candidate merge admission — that is reviewledger.Admit.
package preflight

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"gopkg.in/yaml.v3"
)

// Keep the preflight package's public names source-compatible while the
// configuration package owns the single on-disk representation.
type MergePolicy = config.MergePolicy
type RemoteCIPolicy = config.RemoteCIPolicy

// DefaultProtectedPolicy is the fail-closed baseline when merge_policy is
// omitted from herd.yaml.
func DefaultProtectedPolicy() MergePolicy {
	return MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"Build, Preflight & Test Suite"},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	}
}

// LoadMergePolicy reads merge_policy from .herd/herd.yaml. Missing config or
// omitted merge_policy yields DefaultProtectedPolicy so absence cannot
// silently open autonomous merge.
func LoadMergePolicy(root string) (MergePolicy, error) {
	path := config.PathFor(root)
	if root == "" || root == "." {
		path = config.RuntimeConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultProtectedPolicy(), nil
		}
		return MergePolicy{}, fmt.Errorf("read merge policy: %w", err)
	}
	var envelope struct {
		MergePolicy *MergePolicy `yaml:"merge_policy"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return MergePolicy{}, fmt.Errorf("parse merge policy %s: %w", path, err)
	}
	if envelope.MergePolicy == nil {
		legacy := filepath.Join(filepath.Dir(path), "merge-policy.yaml")
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return MergePolicy{}, fmt.Errorf("legacy merge policy %s is still present; move its contents under merge_policy in %s", legacy, path)
		} else if !os.IsNotExist(legacyErr) {
			return MergePolicy{}, fmt.Errorf("inspect legacy merge policy %s: %w", legacy, legacyErr)
		}
		return DefaultProtectedPolicy(), nil
	}
	return *envelope.MergePolicy, nil
}

// PolicyRevision is a stable content digest used to bind remote CI evidence
// to the exact merge contract that was active when it was settled.
func PolicyRevision(policy MergePolicy) string {
	canonical := policy
	canonical.RequiredChecks = normalizeNames(policy.RequiredChecks)
	canonical.RemoteCI.RequiredChecks = normalizeNames(policy.RemoteCI.RequiredChecks)
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return "merge-policy-v2:" + hex.EncodeToString(sum[:])
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
	if policy.RemoteCI.Required && len(normalizeNames(policy.RemoteCI.RequiredChecks)) == 0 {
		rep.OK = false
		rep.Reasons = append(rep.Reasons, "protected repository declares no remote_ci.required_checks")
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
