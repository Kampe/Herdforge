package envelope

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const sigPrefix = "sha256="

// Canonical returns the deterministic byte sequence that is MAC'd. Signature
// is excluded. Field order is fixed so neither JSON key reordering nor body
// splicing can preserve a captured MAC.
//
// Format (newline-separated, no trailing newline after last field):
//
//	v1
//	id=...
//	kind=...
//	seq=...
//	nonce=...
//	issuer_role=...
//	issuer_session=...
//	policy_authority=...
//	target_task=...
//	lease_generation=...
//	target_worker_session=...
//	issued_at=...
//	expires_at=...
//	body_sha256=...
//	scope=...
func (e *Envelope) Canonical() []byte {
	bodySum := sha256.Sum256([]byte(e.Body))
	var b strings.Builder
	b.Grow(512)
	b.WriteString("v1\n")
	b.WriteString("id=")
	b.WriteString(e.ID)
	b.WriteByte('\n')
	b.WriteString("kind=")
	b.WriteString(string(e.Kind))
	b.WriteByte('\n')
	b.WriteString("seq=")
	b.WriteString(strconv.FormatUint(e.Sequence, 10))
	b.WriteByte('\n')
	b.WriteString("nonce=")
	b.WriteString(e.Nonce)
	b.WriteByte('\n')
	b.WriteString("issuer_role=")
	b.WriteString(e.IssuerRole)
	b.WriteByte('\n')
	b.WriteString("issuer_session=")
	b.WriteString(e.IssuerSession)
	b.WriteByte('\n')
	b.WriteString("policy_authority=")
	b.WriteString(e.PolicyAuthority)
	b.WriteByte('\n')
	b.WriteString("target_task=")
	b.WriteString(e.TargetTask)
	b.WriteByte('\n')
	b.WriteString("lease_generation=")
	b.WriteString(strconv.FormatInt(e.LeaseGeneration, 10))
	b.WriteByte('\n')
	b.WriteString("target_worker_session=")
	b.WriteString(e.TargetWorkerSession)
	b.WriteByte('\n')
	b.WriteString("issued_at=")
	b.WriteString(strconv.FormatInt(e.IssuedAtUnix, 10))
	b.WriteByte('\n')
	b.WriteString("expires_at=")
	b.WriteString(strconv.FormatInt(e.ExpiresAtUnix, 10))
	b.WriteByte('\n')
	b.WriteString("body_sha256=")
	b.WriteString(hex.EncodeToString(bodySum[:]))
	b.WriteByte('\n')
	b.WriteString("scope=")
	b.WriteString(scopeCanonical(e.Scope))
	return []byte(b.String())
}

// scopeCanonical is a stable, delimiter-safe encoding of Scope for MAC input.
func scopeCanonical(s *Scope) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("exclusive=")
	if s.Exclusive {
		b.WriteString("1")
	} else {
		b.WriteString("0")
	}
	b.WriteString(";note=")
	// Escape separators so note text cannot inject extra fields.
	b.WriteString(escapeField(s.Note))
	b.WriteString(";pkgs=")
	for i, p := range s.PackageAllowlist {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(escapeField(p))
	}
	return b.String()
}

func escapeField(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`;`, `\;`,
		`,`, `\,`,
		`=`, `\=`,
		"\n", `\n`,
	)
	return r.Replace(s)
}

// Sign attaches Signature using HMAC-SHA256 over Canonical(). Fail-closed on
// empty secret or incomplete envelope.
func Sign(secret []byte, e *Envelope) error {
	if len(secret) == 0 {
		return ErrMissingSecret
	}
	if err := e.ValidateUnsigned(); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(e.Canonical())
	e.Signature = sigPrefix + hex.EncodeToString(mac.Sum(nil))
	return nil
}

// VerifyMAC reports whether e.Signature is a valid HMAC-SHA256 of Canonical()
// under secret. Empty secret, empty/malformed signature, or MAC mismatch all
// return false (fail-closed). Comparison is constant-time via hmac.Equal.
func VerifyMAC(secret []byte, e *Envelope) bool {
	if len(secret) == 0 || e == nil || e.Signature == "" {
		return false
	}
	if !strings.HasPrefix(e.Signature, sigPrefix) {
		return false
	}
	sig, err := hex.DecodeString(e.Signature[len(sigPrefix):])
	if err != nil || len(sig) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(e.Canonical())
	return hmac.Equal(sig, mac.Sum(nil))
}

// FormatSig builds a "sha256=<hex>" signature string (tests / tooling).
func FormatSig(sum []byte) string {
	return sigPrefix + hex.EncodeToString(sum)
}
