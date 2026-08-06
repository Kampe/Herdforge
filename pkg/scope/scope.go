// Package scope binds admission, review, and merge decisions to the complete
// merge-base..candidate delta instead of a single commit or a candidate's
// direct parent.
//
// FAC-69 incident this closes: a candidate was compared only against its
// direct parent and looked package-green, while the true target-branch
// merge base showed the full six-commit delta had introduced a regression
// five commits earlier that was never caught. A receipt scoped to
// HEAD^..HEAD cannot stand in for one covering merge-base..HEAD.
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const Version1 = 1

// AdmissionScope is the immutable, cryptographically bound description of
// everything between a target branch's current tip and one candidate
// commit: the merge base, the exact ordered commit chain, the paths it
// touches, and a digest over the full range diff. Gates must recompute this
// and compare it against a recorded scope (VerifyCurrent) rather than trust
// a stale or narrower comparison.
type AdmissionScope struct {
	Version            int      `json:"version"`
	Repository         string   `json:"repository"`
	TargetBranch       string   `json:"target_branch"`
	TargetSHA          string   `json:"target_sha"`
	CandidateRef       string   `json:"candidate_ref,omitempty"`
	CandidateSHA       string   `json:"candidate_sha"`
	CandidateTree      string   `json:"candidate_tree"`
	MergeBase          string   `json:"merge_base"`
	Commits            []string `json:"commits"`
	ChangedPaths       []string `json:"changed_paths"`
	DiffDigest         string   `json:"diff_digest"`
	GraphBuildIdentity string   `json:"graph_build_identity,omitempty"`
	Digest             string   `json:"digest"`
}

type scopeForDigest struct {
	Version            int      `json:"version"`
	Repository         string   `json:"repository"`
	TargetBranch       string   `json:"target_branch"`
	TargetSHA          string   `json:"target_sha"`
	CandidateRef       string   `json:"candidate_ref,omitempty"`
	CandidateSHA       string   `json:"candidate_sha"`
	CandidateTree      string   `json:"candidate_tree"`
	MergeBase          string   `json:"merge_base"`
	Commits            []string `json:"commits"`
	ChangedPaths       []string `json:"changed_paths"`
	DiffDigest         string   `json:"diff_digest"`
	GraphBuildIdentity string   `json:"graph_build_identity,omitempty"`
}

// Sentinel errors identifying exactly which scope invariant failed, so
// callers can fail closed on the specific condition without parsing prose.
var (
	ErrTargetAdvanced   = errors.New("scope: target branch advanced")
	ErrMergeBaseChanged = errors.New("scope: merge base changed")
	ErrCommitSetChanged = errors.New("scope: commit set changed")
	ErrForcePushed      = errors.New("scope: candidate ref no longer resolves to the recorded candidate sha")
	ErrPathSetChanged   = errors.New("scope: changed path set drifted")
	ErrCandidateMissing = errors.New("scope: candidate commit is no longer reachable")
	ErrScopeMismatch    = errors.New("scope: recorded scope does not match recomputed scope")
	ErrScopeMissing     = errors.New("scope: no recorded admission scope for candidate")
)

func computeDigest(s AdmissionScope) string {
	payload := scopeForDigest{
		Version:            s.Version,
		Repository:         s.Repository,
		TargetBranch:       s.TargetBranch,
		TargetSHA:          s.TargetSHA,
		CandidateRef:       s.CandidateRef,
		CandidateSHA:       s.CandidateSHA,
		CandidateTree:      s.CandidateTree,
		MergeBase:          s.MergeBase,
		Commits:            append([]string(nil), s.Commits...),
		ChangedPaths:       append([]string(nil), s.ChangedPaths...),
		DiffDigest:         s.DiffDigest,
		GraphBuildIdentity: s.GraphBuildIdentity,
	}
	data, _ := json.Marshal(payload)
	return "sha256:" + digestBytes(data)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SelfValidate reports whether s.Digest matches the digest recomputed from
// its own fields, catching a tampered or hand-built scope before it is ever
// compared against a fresh git resolution.
func (s AdmissionScope) SelfValidate() error {
	if s.Digest == "" {
		return ErrScopeMissing
	}
	if computeDigest(s) != s.Digest {
		return ErrScopeMismatch
	}
	return nil
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
