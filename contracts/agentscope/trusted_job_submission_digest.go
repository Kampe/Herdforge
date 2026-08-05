package agentscope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// DecodeTrustedJobSubmissionStrict rejects unknown fields and trailing JSON.
// In particular, it rejects inline configuration YAML, manifests, paths,
// floating refs, and raw credential material because none are part of the
// outbound admission contract.
func DecodeTrustedJobSubmissionStrict(r io.Reader) (TrustedJobSubmission, error) {
	var submission TrustedJobSubmission
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&submission); err != nil {
		return TrustedJobSubmission{}, fmt.Errorf("decode trusted job submission: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return TrustedJobSubmission{}, fmt.Errorf("decode trusted job submission: trailing JSON value")
		}
		return TrustedJobSubmission{}, fmt.Errorf("decode trusted job submission trailing data: %w", err)
	}
	return submission, nil
}

// CanonicalTrustedJobSubmissionJSON returns a stable RFC 8785 JCS encoding for
// submission auditing. The embedded AgentScope, virtual credential scopes, and
// evidence kinds are normalized before encoding; caller-owned data is never
// mutated.
func CanonicalTrustedJobSubmissionJSON(submission TrustedJobSubmission) ([]byte, error) {
	normalized := submission
	normalized.Scope = normalizeSetOrdering(submission.Scope)
	normalized.VirtualCredentials = normalizeVirtualCredentials(submission.VirtualCredentials)
	normalized.Evidence.Kinds = sortedEvidenceKinds(submission.Evidence.Kinds)

	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("canonicalize trusted job submission: %w", err)
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("canonicalize trusted job submission: %w", err)
	}
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, tree); err != nil {
		return nil, fmt.Errorf("canonicalize trusted job submission: %w", err)
	}
	return buf.Bytes(), nil
}

// TrustedJobSubmissionDigest returns the canonical submission SHA-256 identity.
// This digest is the binding token inbound callbacks must reference.
func TrustedJobSubmissionDigest(submission TrustedJobSubmission) (string, error) {
	canonical, err := CanonicalTrustedJobSubmissionJSON(submission)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalizeVirtualCredentials returns a deep copy of refs with scopes sorted
// and refs ordered by (ref, scopes) for a stable canonical identity. The
// caller-owned slice is never mutated.
func normalizeVirtualCredentials(refs []VirtualCredentialRef) []VirtualCredentialRef {
	out := make([]VirtualCredentialRef, len(refs))
	for i, ref := range refs {
		out[i] = VirtualCredentialRef{Ref: ref.Ref, Scopes: sortedStrings(ref.Scopes)}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		if len(out[i].Scopes) != len(out[j].Scopes) {
			return len(out[i].Scopes) < len(out[j].Scopes)
		}
		for k := range out[i].Scopes {
			if out[i].Scopes[k] != out[j].Scopes[k] {
				return out[i].Scopes[k] < out[j].Scopes[k]
			}
		}
		return false
	})
	return out
}
