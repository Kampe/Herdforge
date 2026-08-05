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

// DecodeTrustedJobCallbackStrict rejects unknown fields and trailing JSON.
// In particular, it rejects inline job YAML, manifests, paths, refs, and raw
// credential fields because none are part of the trusted-job contract.
func DecodeTrustedJobCallbackStrict(r io.Reader) (TrustedJobCallbackRequest, error) {
	var request TrustedJobCallbackRequest
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil {
		return TrustedJobCallbackRequest{}, fmt.Errorf("decode trusted job callback: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return TrustedJobCallbackRequest{}, fmt.Errorf("decode trusted job callback: trailing JSON value")
		}
		return TrustedJobCallbackRequest{}, fmt.Errorf("decode trusted job callback trailing data: %w", err)
	}
	return request, nil
}

// CanonicalTrustedJobCallbackJSON returns a stable RFC 8785 JCS encoding for
// callback auditing. The embedded AgentScope and set-like evidence records are
// normalized before encoding; caller-owned data is never mutated.
func CanonicalTrustedJobCallbackJSON(request TrustedJobCallbackRequest) ([]byte, error) {
	normalized := request
	normalized.Scope = normalizeSetOrdering(request.Scope)
	normalized.Evidence = append([]TrustedJobEvidence(nil), request.Evidence...)
	sort.SliceStable(normalized.Evidence, func(i, j int) bool {
		if normalized.Evidence[i].Kind != normalized.Evidence[j].Kind {
			return normalized.Evidence[i].Kind < normalized.Evidence[j].Kind
		}
		return normalized.Evidence[i].Digest < normalized.Evidence[j].Digest
	})

	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("canonicalize trusted job callback: %w", err)
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("canonicalize trusted job callback: %w", err)
	}
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, tree); err != nil {
		return nil, fmt.Errorf("canonicalize trusted job callback: %w", err)
	}
	return buf.Bytes(), nil
}

// TrustedJobCallbackDigest returns the canonical callback SHA-256 identity.
// This is distinct from ScopeDigest, which always identifies only AgentScope.
func TrustedJobCallbackDigest(request TrustedJobCallbackRequest) (string, error) {
	canonical, err := CanonicalTrustedJobCallbackJSON(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
