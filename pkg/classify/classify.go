// Package classify provides deterministic R0–R3 risk classification with
// machine-readable evidence. Classification is bound to a candidate SHA and
// optional patch ID, independent of execution backend and model-family identity.
//
// Unknown or ambiguous scope fails upward (never downward). Mixed diffs take
// the highest applicable tier. FAC-80; FAC-142 consumes the evidence output.
package classify

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ClassifierVersion is the rule-set identity. Bump when rule semantics change
// so consumers can invalidate cached verdicts.
const ClassifierVersion = "1.0.0"

// Tier is the R0–R3 risk level.
type Tier string

const (
	TierR0 Tier = "R0" // documentation / purely mechanical metadata
	TierR1 Tier = "R1" // tests, bounded low-risk, generated-only
	TierR2 Tier = "R2" // features, APIs, workflow/state, dependencies
	TierR3 Tier = "R3" // auth, secrets, destructive, core orchestration, infra
)

// rank returns a comparable rank; higher is more severe.
func (t Tier) rank() int {
	switch t {
	case TierR0:
		return 0
	case TierR1:
		return 1
	case TierR2:
		return 2
	case TierR3:
		return 3
	default:
		return 2 // unknown tier string treated as conservative R2-equivalent
	}
}

// Max returns the higher of two tiers.
func Max(a, b Tier) Tier {
	if a.rank() >= b.rank() {
		return a
	}
	return b
}

// ChangeKind describes how a path appears in the diff.
type ChangeKind string

const (
	ChangeAdd    ChangeKind = "add"
	ChangeModify ChangeKind = "modify"
	ChangeDelete ChangeKind = "delete"
	ChangeRename ChangeKind = "rename"
)

// FileChange is one path-level change. OldPath is set for renames.
type FileChange struct {
	Path    string     `json:"path"`
	OldPath string     `json:"old_path,omitempty"`
	Kind    ChangeKind `json:"kind,omitempty"`
}

// Policy holds repository-specific extensions. Provider/model identity must
// never appear here — classification stays backend-agnostic.
type Policy struct {
	// CoreOrchestrationPrefixes elevate matching paths to R3 (e.g. "pkg/daemon/").
	CoreOrchestrationPrefixes []string `json:"core_orchestration_prefixes,omitempty"`
	// ExtraR3Substrings force R3 when present in a normalized path.
	ExtraR3Substrings []string `json:"extra_r3_substrings,omitempty"`
	// ExtraR2Substrings force at least R2 when present in a normalized path.
	ExtraR2Substrings []string `json:"extra_r2_substrings,omitempty"`
	// ExtraR1Substrings force at least R1 when present in a normalized path.
	ExtraR1Substrings []string `json:"extra_r1_substrings,omitempty"`
}

// DefaultPolicy returns Herdforge-oriented core orchestration defaults.
// Callers may replace or extend this without provider-specific logic.
func DefaultPolicy() Policy {
	return Policy{
		CoreOrchestrationPrefixes: []string{
			"pkg/daemon/",
			"pkg/dispatch/",
			"pkg/claim/",
			"pkg/worker/",
			"pkg/lifecycle/",
			"pkg/outbox/",
			"pkg/router/",
			"pkg/worktree/",
			"pkg/verifier/",
			"cmd/herd/",
		},
	}
}

// Input is the classifier request. CandidateSHA binds the verdict to a revision.
type Input struct {
	CandidateSHA string       `json:"candidate_sha"`
	PatchID      string       `json:"patch_id,omitempty"`
	Paths        []string     `json:"paths,omitempty"`
	Changes      []FileChange `json:"changes,omitempty"`
	// Symbols are changed public/exported identifiers (optional).
	Symbols []string `json:"symbols,omitempty"`
	// Policy optional; nil uses DefaultPolicy().
	Policy *Policy `json:"policy,omitempty"`
}

// RuleMatch is one fired classification rule with evidence.
type RuleMatch struct {
	ID     string   `json:"id"`
	Tier   Tier     `json:"tier"`
	Reason string   `json:"reason"`
	Paths  []string `json:"paths,omitempty"`
}

// Result is the machine-readable classification verdict.
type Result struct {
	Tier              Tier        `json:"tier"`
	Rules             []RuleMatch `json:"rules"`
	ChangedPaths      []string    `json:"changed_paths"`
	ChangedSymbols    []string    `json:"changed_symbols,omitempty"`
	RequiredGates     []string    `json:"required_gates"`
	ClassifierVersion string      `json:"classifier_version"`
	CandidateSHA      string      `json:"candidate_sha"`
	PatchID           string      `json:"patch_id,omitempty"`
	Reasons           []string    `json:"reasons"`
}

// ValidFor reports whether this result still applies to the given revision.
// Any SHA mismatch or (when both sides set a patch ID) patch mismatch invalidates.
func (r Result) ValidFor(candidateSHA, patchID string) bool {
	if r.CandidateSHA == "" || candidateSHA == "" || r.CandidateSHA != candidateSHA {
		return false
	}
	if r.ClassifierVersion != ClassifierVersion {
		return false
	}
	if r.PatchID != "" && patchID != "" && r.PatchID != patchID {
		return false
	}
	return true
}

// MarshalJSON produces stable JSON (rules/paths/gates already sorted by Classify).
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	return json.Marshal(alias(r))
}

// Classify returns a deterministic risk tier and evidence for the input.
// Path order and map iteration do not affect the result.
func Classify(in Input) Result {
	pol := DefaultPolicy()
	if in.Policy != nil {
		pol = *in.Policy
	}

	paths := collectPaths(in)
	symbols := uniqueSorted(in.Symbols)

	rules := evaluateRules(paths, in.Changes, symbols, pol)
	tier := TierR0
	if len(rules) == 0 {
		// Ambiguous / empty scope fails upward.
		rules = []RuleMatch{{
			ID:     "unknown.conservative",
			Tier:   TierR2,
			Reason: "no matching rules or empty/unknown scope; fail upward",
			Paths:  append([]string(nil), paths...),
		}}
	}
	for _, rm := range rules {
		tier = Max(tier, rm.Tier)
	}

	// Keep only rules that contribute at the winning tier or document lower
	// evidence? Spec wants full evidence of matched rules — keep all matches.
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].ID != rules[j].ID {
			return rules[i].ID < rules[j].ID
		}
		return rules[i].Reason < rules[j].Reason
	})

	reasons := make([]string, 0, len(rules))
	for _, rm := range rules {
		reasons = append(reasons, fmt.Sprintf("%s:%s:%s", rm.ID, rm.Tier, rm.Reason))
	}

	return Result{
		Tier:              tier,
		Rules:             rules,
		ChangedPaths:      paths,
		ChangedSymbols:    symbols,
		RequiredGates:     GatesFor(tier),
		ClassifierVersion: ClassifierVersion,
		CandidateSHA:      in.CandidateSHA,
		PatchID:           in.PatchID,
		Reasons:           reasons,
	}
}

func collectPaths(in Input) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		p = normalizePath(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range in.Paths {
		add(p)
	}
	for _, c := range in.Changes {
		add(c.Path)
		if c.OldPath != "" {
			add(c.OldPath)
		}
	}
	sort.Strings(out)
	return out
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = filepath.ToSlash(p)
	// Strip leading ./ for stable matching.
	p = strings.TrimPrefix(p, "./")
	return p
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
