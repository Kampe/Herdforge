package agentscope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
)

// CanonicalizationProfile documents the exact canonicalization scheme used to
// compute a cross-language reproducible AgentScope digest. Implementations in
// any language MUST produce byte-identical output for the same logical scope.
//
// Profile: RFC 8785 JSON Canonicalization Scheme (JCS), with the following
// pinning decisions required for reproducibility across Go and TypeScript:
//
//  1. Object member keys are sorted in ascending order by UTF-16 code unit
//     (RFC 8785 §3.2.3). Sorting is performed on the UTF-16 encoding of keys,
//     NOT on the Go struct declaration order or raw UTF-8 bytes, so a
//     BMP character above U+FFFF would sort after its UTF-16 surrogate pair.
//  2. Arrays preserve element order (RFC 8785 §3.2.2). Set-like arrays in
//     AgentScope (paths, commandProfiles, allowedHosts, git.actions, ...) are
//     sorted at the application layer by CanonicalJSON BEFORE canonical
//     encoding, so array order is itself a stable, normalized part of the
//     contract identity rather than an input-dependent artifact.
//  3. Numbers are serialized in the shortest form that round-trips to the
//     same IEEE 754 double (RFC 8785 §3.2.2.3). Go's encoding/json float
//     encoder already produces this minimal form; integers are emitted
//     without a decimal point or exponent. NaN/Inf are not valid JSON and
//     are rejected before canonicalization.
//  4. Strings are escaped per RFC 8785 §3.2.4: only U+0000–U+001F, the
//     reverse-solidus `\`, and the quotation mark `"` are escaped. The
//     solidus `/` is NOT escaped. Non-ASCII BMP and astral characters are
//     emitted as raw UTF-8, never as `\uXXXX` escapes.
//  5. No insignificant whitespace, no trailing newline, UTF-8 output.
//
// This profile deliberately does NOT depend on Go struct field declaration
// order: the canonical encoder operates on a decoded generic JSON tree, so a
// TypeScript implementation using the same JCS profile produces identical
// bytes without any knowledge of the Go type system.
const CanonicalizationProfile = "RFC 8785 JCS (UTF-16 key sort, minimal numbers, raw UTF-8, no solidus escape)"

// DecodeStrict rejects unknown fields and trailing JSON so raw credentials or
// inline authority cannot hide outside the typed v1alpha1 contract.
func DecodeStrict(r io.Reader) (AgentScope, error) {
	var scope AgentScope
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		return AgentScope{}, fmt.Errorf("decode AgentScope: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return AgentScope{}, fmt.Errorf("decode AgentScope: trailing JSON value")
		}
		return AgentScope{}, fmt.Errorf("decode AgentScope trailing data: %w", err)
	}
	return scope, nil
}

// CanonicalJSON returns a stable, cross-language contract encoding. Set-like
// arrays are sorted on a deep copy first (so caller order is not part of the
// policy identity), then the value is serialized under the RFC 8785 JCS
// profile documented in CanonicalizationProfile. The caller-owned scope is
// never mutated.
func CanonicalJSON(scope AgentScope) ([]byte, error) {
	normalized := normalizeSetOrdering(scope)
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("canonicalize AgentScope: %w", err)
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("canonicalize AgentScope: %w", err)
	}
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest computes the canonical SHA-256 contract identity under the profile
// documented in CanonicalizationProfile. The returned digest is prefixed with
// "sha256:" and is reproducible by any conformant TypeScript JCS impl.
func Digest(scope AgentScope) (string, error) {
	canonical, err := CanonicalJSON(scope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CanonicalDigestHex returns the bare hex SHA-256 (no "sha256:" prefix) of the
// canonical encoding, plus the canonical bytes. It is intended for golden
// fixture generation and cross-language parity tests.
func CanonicalDigestHex(scope AgentScope) (hexDigest string, canonical []byte, err error) {
	canonical, err = CanonicalJSON(scope)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}

func normalizeSetOrdering(scope AgentScope) AgentScope {
	canonical := scope
	canonical.Spec.Paths.Readable = sortedStrings(scope.Spec.Paths.Readable)
	canonical.Spec.Paths.Writable = sortedStrings(scope.Spec.Paths.Writable)
	canonical.Spec.CommandProfiles = sortedStrings(scope.Spec.CommandProfiles)
	canonical.Spec.Network.AllowedHosts = sortedStrings(scope.Spec.Network.AllowedHosts)
	canonical.Spec.Git.Actions = sortedGitActions(scope.Spec.Git.Actions)
	canonical.Spec.Credentials = make([]CredentialGrant, len(scope.Spec.Credentials))
	copy(canonical.Spec.Credentials, scope.Spec.Credentials)
	for i := range canonical.Spec.Credentials {
		canonical.Spec.Credentials[i].Scopes = sortedStrings(canonical.Spec.Credentials[i].Scopes)
	}
	sort.SliceStable(canonical.Spec.Credentials, func(i, j int) bool {
		if canonical.Spec.Credentials[i].Ref != canonical.Spec.Credentials[j].Ref {
			return canonical.Spec.Credentials[i].Ref < canonical.Spec.Credentials[j].Ref
		}
		if canonical.Spec.Credentials[i].Provider != canonical.Spec.Credentials[j].Provider {
			return canonical.Spec.Credentials[i].Provider < canonical.Spec.Credentials[j].Provider
		}
		return canonical.Spec.Credentials[i].ExpiresAt < canonical.Spec.Credentials[j].ExpiresAt
	})
	canonical.Spec.Evidence.Kinds = sortedEvidenceKinds(scope.Spec.Evidence.Kinds)
	canonical.Spec.Grants.Extensions = sortedStrings(scope.Spec.Grants.Extensions)
	return canonical
}

// encodeCanonical writes a value under the RFC 8785 JCS profile. It accepts
// the subset of Go types produced by encoding/json with UseNumber: bool,
// json.Number, string, []any, map[string]any, and nil.
func encodeCanonical(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		if err := encodeCanonicalNumber(buf, val); err != nil {
			return err
		}
	case string:
		encodeCanonicalString(buf, val)
	case []any:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.SliceStable(keys, func(i, j int) bool {
			return utf16Less(keys[i], keys[j])
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := encodeCanonical(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonicalize AgentScope: unsupported type %T", v)
	}
	return nil
}

// utf16Less reports whether a < b under UTF-16 code unit ordering, the key
// sort required by RFC 8785 §3.2.3. Astral characters encode to surrogate
// pairs in UTF-16, so they sort after the BMP, which differs from raw UTF-8
// byte ordering for code points above U+FFFF.
func utf16Less(a, b string) bool {
	au := utf16.Encode([]rune(a))
	bu := utf16.Encode([]rune(b))
	n := len(au)
	if len(bu) < n {
		n = len(bu)
	}
	for i := 0; i < n; i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

// encodeCanonicalNumber emits the shortest round-tripping IEEE 754 double
// form per RFC 8785 §3.2.2.3. json.Number from UseNumber preserves the input
// lexical form, so we re-derive the minimal form via float64 round-tripping.
// Integers emit without a decimal point or exponent. NaN/Inf are rejected.
func encodeCanonicalNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	f, err := n.Float64()
	if err != nil {
		return fmt.Errorf("canonicalize AgentScope: invalid number %q: %w", s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("canonicalize AgentScope: non-finite number %q", s)
	}
	if isIntegerFloat(f) && withinCanonicalIntegerRange(f) {
		buf.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// isIntegerFloat reports whether f is a whole number with no fractional part.
func isIntegerFloat(f float64) bool {
	return f == math.Trunc(f) && !math.IsInf(f, 0)
}

// withinCanonicalIntegerRange reports whether f fits the signed 64-bit range
// that JCS permits to serialize as a bare integer. Values outside this range
// must fall back to the shortest float form to stay lossless and reproducible.
func withinCanonicalIntegerRange(f float64) bool {
	return f >= -9.2233720368547758e+18 && f <= 9.2233720368547758e+18
}

func encodeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r < 0x20:
			fmt.Fprintf(buf, `\u%04x`, r)
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

func sortedStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	sort.Strings(out)
	return out
}

func sortedGitActions(values []GitAction) []GitAction {
	out := make([]GitAction, len(values))
	copy(out, values)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedEvidenceKinds(values []EvidenceKind) []EvidenceKind {
	out := make([]EvidenceKind, len(values))
	copy(out, values)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
