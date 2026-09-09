// Typed broker-verdict retention and recovery (FAC-351 seam, restored by
// FAC-740).
//
// The FAC-145 broker verdict path records a non-consumable intent, delivers
// the signed effect to the provider, and publishes a consumable callback.
// Between the confirmed delivery readback and that consumable callback, the
// exact-SHA verdict must also be retained as a canonical, content-addressed
// review-inbox artifact — otherwise a delivered verdict resolves leases but
// leaves no review authority behind (the shipped regression: FAC-737 seq587
// and FAC-738 seq584 produced applied=[] with no inbox or ledger entry).
//
// Recovery is the second half of the seam: a delivered provider effect or a
// delivered bus callback whose artifact is missing can be materialized
// EXACTLY ONCE into the same canonical format — but only from evidence that
// carries the coordinator's signature over the full effect identity. An
// intent-only record (FAC-739 gen1 seq592) is never admissible: it was never
// delivered, never signed as an effect, and any later delivered verdict for
// the same candidate supersedes it.
package reviewingest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// VerdictEffectLinePrefix begins every broker-composed verdict line.
const VerdictEffectLinePrefix = "REVIEW VERDICT "

// effectTrailerParts are the fixed phrases the broker appends after the typed
// fields and around the effect trailer.
const (
	effectBoundSuffix = " reviewer-bound (FAC-145)"
	effectTrailerOpen = " [effect "
	effectSigJoin     = " sig="
)

// VerdictEffect is the parsed, signed delivered-verdict effect.
type VerdictEffect struct {
	// TaskRef is the task the verdict was composed for.
	TaskRef string
	// VerdictToken is the broker token: APPROVED or REJECTED.
	VerdictToken string
	// Candidate and Base are the exact commits the signed effect binds.
	Candidate string
	Base      string
	// LeaseID and LeaseGeneration are the review lease identity inside the
	// signature.
	LeaseID         string
	LeaseGeneration int64
	// Detail is the reviewer detail the broker composed into the line, "" when none.
	Detail string
	// EffectID is the full delivered identity (verdict-delivered:...).
	EffectID string
	// Signature is the coordinator's hex signature over
	// "herd-verdict-effect:" + EffectID + "\n" + Line.
	Signature string
	// Line is the canonical verdict line without the effect trailer.
	Line string
	// Body is the exact delivered provider comment body.
	Body string
}

// LedgerVerdict maps the broker's verdict token onto the ledger verdict the
// canonical artifact records. APPROVED admits work (PASS); REJECTED vetoes it
// (FAIL). Anything else was never a broker verdict.
func LedgerVerdict(token string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(token)) {
	case "APPROVED":
		return "PASS", nil
	case "REJECTED":
		return "FAIL", nil
	default:
		return "", fmt.Errorf("verdict token %q is not a broker verdict (APPROVED|REJECTED)", token)
	}
}

// ParseVerdictEffect splits and parses the exact delivered provider comment
// body into its typed effect. It refuses intent records, unsigned bodies, and
// anything whose shape the broker could not have composed.
func ParseVerdictEffect(body string) (VerdictEffect, error) {
	var e VerdictEffect
	body = strings.TrimRight(body, "\r\n")
	e.Body = body

	// The effect trailer is the LAST bracketed suffix the broker composed.
	idx := strings.LastIndex(body, effectTrailerOpen)
	if idx < 0 || !strings.HasSuffix(body, "]") {
		return e, fmt.Errorf("delivered verdict body carries no effect trailer — an intent or hand-written comment is not a delivered effect (FAC-351)")
	}
	trailer := body[idx+len(effectTrailerOpen) : len(body)-1]
	si := strings.Index(trailer, effectSigJoin)
	if si <= 0 {
		return e, fmt.Errorf("effect trailer carries no signature (FAC-351 fail-closed)")
	}
	e.EffectID = trailer[:si]
	e.Signature = trailer[si+len(effectSigJoin):]
	if strings.ContainsAny(e.EffectID, " \t") || !strings.HasPrefix(e.EffectID, mail.VerdictDeliveredPrefix) {
		return e, fmt.Errorf("effect identity %q is not a delivered verdict id (FAC-351)", e.EffectID)
	}
	if _, err := hex.DecodeString(e.Signature); err != nil || e.Signature == "" {
		return e, fmt.Errorf("effect signature is not hex — refusing unsigned or corrupted evidence (FAC-351 fail-closed)")
	}
	e.Line = body[:idx]

	rest, ok := strings.CutPrefix(e.Line, VerdictEffectLinePrefix)
	if !ok {
		return e, fmt.Errorf("delivered effect line is not a broker-composed verdict (FAC-145)")
	}
	// Detail, when present, was appended once at the very end; split on its
	// last occurrence so the fixed-field grammar stays authoritative.
	detail := ""
	if di := strings.LastIndex(rest, " — "); di >= 0 {
		detail = strings.TrimSpace(rest[di+len(" — "):])
		rest = rest[:di]
	}
	e.Detail = detail
	if !strings.HasSuffix(rest, effectBoundSuffix) {
		return e, fmt.Errorf("delivered verdict line lacks the broker binding suffix (FAC-145)")
	}
	rest = strings.TrimSuffix(rest, effectBoundSuffix)

	task, fields, ok := strings.Cut(rest, ": ")
	if !ok {
		return e, fmt.Errorf("delivered verdict line carries no task ref (FAC-145)")
	}
	e.TaskRef = strings.TrimSpace(task)
	if e.TaskRef == "" || strings.ContainsAny(e.TaskRef, " \t") {
		return e, fmt.Errorf("delivered verdict task ref %q is not a task identity", e.TaskRef)
	}

	// The remaining fields are positional: <verdict> candidate=<sha> base=<sha>
	// lease=<id> lease_gen=<gen>.
	words := strings.Fields(strings.TrimSpace(fields))
	if len(words) != 5 {
		return e, fmt.Errorf("delivered verdict line has %d typed fields, want 5 (FAC-145)", len(words))
	}
	e.VerdictToken = words[0]
	if _, err := LedgerVerdict(e.VerdictToken); err != nil {
		return e, fmt.Errorf("delivered verdict token is not a broker verdict: %w", err)
	}
	want := []string{"candidate=", "base=", "lease=", "lease_gen="}
	vals := make(map[string]string, len(want))
	for i, w := range want {
		v, ok := strings.CutPrefix(words[i+1], w)
		if !ok || v == "" || strings.Contains(v, "=") {
			return e, fmt.Errorf("delivered verdict field %d is not %s<value> (FAC-145)", i+1, w)
		}
		vals[w] = v
	}
	if !isCommitHex(vals["candidate="]) || !isCommitHex(vals["base="]) {
		return e, fmt.Errorf("delivered verdict binds a non-commit candidate or base (FAC-145 fail-closed)")
	}
	gen, err := strconv.ParseInt(vals["lease_gen="], 10, 64)
	if err != nil || gen < 1 {
		return e, fmt.Errorf("delivered verdict lease_gen %q is not a positive generation (FAC-145)", vals["lease_gen="])
	}
	e.Candidate = vals["candidate="]
	e.Base = vals["base="]
	e.LeaseID = vals["lease="]
	e.LeaseGeneration = gen
	return e, nil
}

func isCommitHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// VerdictRecoveryRecord is the typed record a recovery run verifies and
// materializes. Repo/ref/candidate/base/lease/generation/verdict MUST match
// the coordinator-signed delivered effect; branch, reviewer identity/family,
// and the verification record are the typed bindings recovery verifies before
// writing anything.
type VerdictRecoveryRecord struct {
	Repo            string `json:"repo"`
	Ref             string `json:"ref"`
	CandidateSHA    string `json:"candidate_sha"`
	BaseSHA         string `json:"base_sha"`
	Branch          string `json:"branch"`
	LeaseID         string `json:"lease_id"`
	LeaseGeneration int64  `json:"lease_generation"`
	// Verdict is the broker token (APPROVED|REJECTED) the delivered effect carries.
	Verdict string `json:"verdict"`
	// Reviewer is the reviewer identity recovery attributes the verdict to.
	Reviewer string `json:"reviewer"`
	// ReviewerFamily must be an allowed vendor family; recovery refuses an
	// unprovable one. Empty is tolerated only for retention runs that write
	// from an already-authenticated receipt (the broker path).
	ReviewerFamily string `json:"reviewer_family,omitempty"`
	// Verification is the reviewer's own record of what it ran; it may be
	// absent, but a stated VerificationDigest must match it exactly.
	Verification       string `json:"verification,omitempty"`
	VerificationDigest string `json:"verification_digest,omitempty"`
	// Detail must equal the detail inside the signed effect when stated.
	Detail string `json:"detail,omitempty"`
	// BusSequence binds the delivered bus callback's sequence; 0 recovers a
	// provider-only effect with no bus binding.
	BusSequence int64 `json:"bus_sequence,omitempty"`
	// CanonicalBody is the EXACT delivered provider comment body.
	CanonicalBody string `json:"canonical_body"`
	// RecordSig is the coordinator signature over RecoveryRecordSigPayload —
	// the authorized authority binding reviewer identity, reviewer family,
	// and the verification digest to this effect. The signed provider effect
	// covers none of these fields, so recovery without a valid record
	// signature could manufacture independent-review metadata around a
	// publicly readable effect; unsigned records are refused (FAC-351).
	RecordSig string `json:"record_sig,omitempty"`
}

// recoveryRecordSigSeparator is a unit-separator join so no delimiter
// ambiguity can move a value across fields in the signed payload.
const recoveryRecordSigSeparator = "\x1f"

// RecoveryRecordSigPayload is the canonical preimage a recovery record's
// record_sig authenticates: the effect identity plus every metadata field
// recovery would otherwise take on trust. Tampering with any covered field —
// reviewer, family, or the verification digest — invalidates the signature.
func RecoveryRecordSigPayload(rec VerdictRecoveryRecord, effectID string) []byte {
	fields := []string{
		rec.Repo, rec.Ref, rec.CandidateSHA, rec.BaseSHA, rec.Branch,
		rec.LeaseID, strconv.FormatInt(rec.LeaseGeneration, 10), rec.Verdict,
		strings.TrimSpace(rec.Reviewer), strings.TrimSpace(rec.ReviewerFamily),
		strings.TrimSpace(rec.VerificationDigest), strconv.FormatInt(rec.BusSequence, 10),
	}
	return []byte("herd-verdict-recovery-record:" + effectID + "\n" + strings.Join(fields, recoveryRecordSigSeparator))
}

// SignRecoveryRecord fills rec.RecordSig with a coordinator signature over
// the canonical record payload (the effect id is derived from the record's
// own canonical body). This is the only sanctioned way to mint a recovery
// record; a record without a verifiable signature is refused by recovery.
func SignRecoveryRecord(rec *VerdictRecoveryRecord, sign func(data []byte) (string, error)) (string, error) {
	if rec == nil {
		return "", fmt.Errorf("recovery record is required")
	}
	effect, err := ParseVerdictEffect(rec.CanonicalBody)
	if err != nil {
		return "", err
	}
	sig, err := sign(RecoveryRecordSigPayload(*rec, effect.EffectID))
	if err != nil {
		return "", fmt.Errorf("sign recovery record: %w", err)
	}
	rec.RecordSig = sig
	return effect.EffectID, nil
}

// VerdictRecoveryOptions wires the verification capabilities recovery needs.
// A nil VerifyEffect, VerifyRecord, or BranchReaches refuses — recovery
// without verification capability writes nothing (fail closed).
type VerdictRecoveryOptions struct {
	// VerifyEffect authenticates the coordinator signature over
	// "herd-verdict-effect:" + effectID + "\n" + line.
	VerifyEffect func(line, effectID, sigHex string) error
	// VerifyRecord authenticates the coordinator signature over a recovery
	// record payload (RecoveryRecordSigPayload) — the authority binding the
	// record's reviewer/verification metadata to the effect.
	VerifyRecord func(data []byte, sigHex string) error
	// BranchReaches reports whether branch contains sha (git ancestry).
	BranchReaches func(branch, sha string) bool
	// LatestDeliveredVerdict returns the LATEST delivered verdict effect for
	// (repo, ref, candidate) from the durable bus: effect id and bus
	// sequence. found=false when the bus holds none.
	LatestDeliveredVerdict func(repo, ref, candidate string) (effectID string, sequence int64, found bool, err error)
	// Coordinators are identities that may never be recorded as reviewers.
	// Nil defaults to reviewledger.DefaultCoordinators.
	Coordinators map[string]struct{}
}

// RetainVerdictArtifact composes the canonical front-matter artifact from the
// typed record and its signed delivered effect, verifies the signature and
// every record/effect binding, and retains it under the repository review
// inbox — content-addressed, mode 0600, exactly once.
//
// The same core serves the broker's fresh-verdict retention (record built
// from the authenticated receipt) and recovery; recovery adds its own gates
// in RecoverVerdictArtifact.
func RetainVerdictArtifact(projectRoot string, rec VerdictRecoveryRecord, opts VerdictRecoveryOptions) (string, error) {
	if opts.VerifyEffect == nil {
		return "", fmt.Errorf("no effect verification capability — refusing to retain a verdict without authenticating its signature (FAC-351 fail-closed)")
	}
	// Typed record completeness first, so every later refusal names a
	// mismatch and not a blank field.
	for _, f := range []struct{ name, val string }{
		{"repo", rec.Repo}, {"ref", rec.Ref}, {"branch", rec.Branch},
		{"candidate_sha", rec.CandidateSHA}, {"base_sha", rec.BaseSHA},
		{"lease_id", rec.LeaseID}, {"reviewer", rec.Reviewer},
	} {
		if strings.TrimSpace(f.val) == "" {
			return "", fmt.Errorf("recovery record %s is required (FAC-351 fail-closed)", f.name)
		}
	}
	if rec.LeaseGeneration < 1 {
		return "", fmt.Errorf("recovery record lease_generation %d is invalid (FAC-351)", rec.LeaseGeneration)
	}
	if !shaRe.MatchString(strings.TrimSpace(rec.CandidateSHA)) || !shaRe.MatchString(strings.TrimSpace(rec.BaseSHA)) {
		return "", fmt.Errorf("recovery record candidate/base must be exact 40-hex commits (FAC-351)")
	}
	if strings.TrimSpace(rec.ReviewerFamily) != "" && !reviewledger.FamilyAllowlist[strings.TrimSpace(rec.ReviewerFamily)] {
		return "", fmt.Errorf("recovery record reviewer-family %q is not an allowed vendor family (FAC-351 fail-closed)", rec.ReviewerFamily)
	}

	effect, err := ParseVerdictEffect(rec.CanonicalBody)
	if err != nil {
		return "", err
	}
	if err := opts.VerifyEffect(effect.Line, effect.EffectID, effect.Signature); err != nil {
		return "", fmt.Errorf("delivered effect signature failed verification — refusing unsigned or foreign evidence (FAC-351 fail-closed): %w", err)
	}

	// Every signed binding must equal the record's claim. A mismatch on any
	// one of them is wrong-SHA / wrong-generation / wrong-task evidence.
	effectTask := reviewledger.CloseableCardRef(effect.TaskRef)
	recTask := reviewledger.CloseableCardRef(rec.Ref)
	if effectTask == "" || !strings.EqualFold(effectTask, recTask) {
		return "", fmt.Errorf("signed effect task %q does not match record task %q (FAC-351 fail-closed)", effect.TaskRef, rec.Ref)
	}
	if !strings.EqualFold(effect.VerdictToken, strings.TrimSpace(rec.Verdict)) {
		return "", fmt.Errorf("signed effect verdict %q does not match record verdict %q (FAC-351 fail-closed)", effect.VerdictToken, rec.Verdict)
	}
	if effect.Candidate != strings.TrimSpace(rec.CandidateSHA) {
		return "", fmt.Errorf("signed effect candidate %s does not match record candidate %s — wrong-SHA evidence refused (FAC-351)", effect.Candidate, rec.CandidateSHA)
	}
	if effect.Base != strings.TrimSpace(rec.BaseSHA) {
		return "", fmt.Errorf("signed effect base %s does not match record base %s (FAC-351 fail-closed)", effect.Base, rec.BaseSHA)
	}
	if effect.LeaseID != strings.TrimSpace(rec.LeaseID) || effect.LeaseGeneration != rec.LeaseGeneration {
		return "", fmt.Errorf("signed effect lease %s gen%d does not match record lease %s gen%d — wrong-generation evidence refused (FAC-351)",
			effect.LeaseID, effect.LeaseGeneration, rec.LeaseID, rec.LeaseGeneration)
	}
	if strings.TrimSpace(rec.Detail) != strings.TrimSpace(effect.Detail) {
		return "", fmt.Errorf("record detail does not equal the detail inside the signed effect (FAC-351 fail-closed)")
	}

	// Verification digest is never synthesized: a stated digest must be the
	// exact digest of the stated verification record, and a stated digest
	// with no record is invented evidence.
	digest := verificationDigestOf(rec.Verification)
	if strings.TrimSpace(rec.VerificationDigest) != strings.TrimSpace(digest) {
		return "", fmt.Errorf("verification digest %q does not match the stated verification record (FAC-351: digests are never synthesized)", rec.VerificationDigest)
	}

	return retainVerdictBytes(projectRoot, rec, effect)
}

// RecoverVerdictArtifact runs the recovery-only gates (record signature,
// reviewer identity, branch reach, supersession order) and then retains the
// canonical artifact.
func RecoverVerdictArtifact(projectRoot string, rec VerdictRecoveryRecord, opts VerdictRecoveryOptions) (string, error) {
	if opts.VerifyRecord == nil || opts.BranchReaches == nil || opts.LatestDeliveredVerdict == nil {
		return "", fmt.Errorf("recovery requires record-signature, branch-reach, and bus-order verification capability (FAC-351 fail-closed)")
	}
	coordinators := opts.Coordinators
	if coordinators == nil {
		coordinators = reviewledger.DefaultCoordinators
	}
	effect, parseErr := ParseVerdictEffect(rec.CanonicalBody)
	if parseErr != nil {
		return "", parseErr
	}
	// AUTHORITY before AUTHORIZATION: the record signature is the only
	// sanctioned binding of reviewer identity/family and the verification
	// digest to this effect. The signed provider effect covers neither, so
	// an unsigned or mismatched record would let any holder of a publicly
	// readable effect manufacture independent-review metadata around it.
	// Unsigned and tampered records are refused before anything else runs.
	if strings.TrimSpace(rec.RecordSig) == "" {
		return "", fmt.Errorf("recovery record carries no record signature — reviewer and verification metadata cannot be bound to the signed effect without signed authority (FAC-351 fail-closed)")
	}
	if err := opts.VerifyRecord(RecoveryRecordSigPayload(rec, effect.EffectID), rec.RecordSig); err != nil {
		return "", fmt.Errorf("recovery record signature failed verification — refusing metadata that is not bound to authorized signed authority (FAC-351 fail-closed): %w", err)
	}
	if strings.TrimSpace(rec.ReviewerFamily) == "" {
		return "", fmt.Errorf("recovery requires a reviewer family — a recovered verdict with no reviewer provenance is not independent review evidence (FAC-351)")
	}
	for c := range coordinators {
		if strings.EqualFold(strings.TrimSpace(rec.Reviewer), strings.TrimSpace(c)) {
			return "", fmt.Errorf("reviewer %q is a coordinator; a coordinator identity can never be recovered as an independent reviewer (FAC-351)", rec.Reviewer)
		}
	}
	if !opts.BranchReaches(strings.TrimSpace(rec.Branch), strings.TrimSpace(rec.CandidateSHA)) {
		return "", fmt.Errorf("record branch %s does not reach candidate %s — refusing a verdict about a tree nobody can produce (FAC-351)",
			rec.Branch, rec.CandidateSHA)
	}
	latestID, latestSeq, found, err := opts.LatestDeliveredVerdict(rec.Repo, rec.Ref, rec.CandidateSHA)
	if err != nil {
		return "", fmt.Errorf("bus verdict state unreadable — refusing recovery (FAC-351 fail-closed): %w", err)
	}
	switch {
	case !found:
		if rec.BusSequence != 0 {
			return "", fmt.Errorf("bound bus callback for effect %s no longer exists — stale evidence refused (FAC-351)", effect.EffectID)
		}
	case latestID != effect.EffectID:
		// A later delivered verdict supersedes this one (FAC-145 supersession
		// order). Recovering the superseded loser would admit a verdict that
		// no longer governs.
		return "", fmt.Errorf("delivered effect %s is superseded by %s (bus seq %d) — refusing superseded evidence (FAC-351)",
			effect.EffectID, latestID, latestSeq)
	default:
		if rec.BusSequence != 0 && rec.BusSequence != latestSeq {
			return "", fmt.Errorf("bound bus sequence %d does not match the delivered record sequence %d — duplicate-conflicting evidence refused (FAC-351)",
				rec.BusSequence, latestSeq)
		}
	}
	return RetainVerdictArtifact(projectRoot, rec, opts)
}

// retainVerdictBytes writes the composed artifact under the canonical review
// inbox. The name binds the EFFECT identity and the content, so a retry
// converges on one artifact and a conflicting retry of the same effect
// collides by name and is refused.
func retainVerdictBytes(projectRoot string, rec VerdictRecoveryRecord, effect VerdictEffect) (string, error) {
	text := composeVerdictArtifact(rec, effect)
	name := verdictRetainedName(rec.CandidateSHA, effect.EffectID)
	rel := filepath.ToSlash(filepath.Join(InboxRel, name))
	dst := filepath.Join(projectRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create review inbox: %w", err)
	}
	// Idempotent same-content hit: this exact effect was already retained for
	// this candidate and reviewer. Never rewrite review evidence.
	if existing, err := os.ReadFile(dst); err == nil {
		sum := sha256.Sum256(existing)
		if fmt.Sprintf("%x", sum)[:8] == contentDigest8(text) {
			if err := confirmRetainedHit(dst); err != nil {
				return "", err
			}
			if info, statErr := os.Stat(dst); statErr != nil || !verdictArtifactMode(info.Mode()) {
				return "", fmt.Errorf("retained artifact %s must exist with mode 0600 (FAC-351 fail-closed)", rel)
			}
			return rel, nil
		}
		return "", fmt.Errorf("retained artifact %s exists with different content — duplicate-conflicting evidence refused (FAC-351)", rel)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat retained artifact: %w", err)
	}
	if err := publishRetainedArtifact(dst, []byte(text)); err != nil {
		if errors.Is(err, ErrRetainedCollision) {
			return "", fmt.Errorf("retained artifact %s exists with different content — duplicate-conflicting evidence refused (FAC-351)", rel)
		}
		return "", err
	}
	return rel, nil
}

// verdictArtifactMode requires owner-only permissions on retained verdict
// evidence.
func verdictArtifactMode(m os.FileMode) bool { return m.Perm() == 0o600 }

func contentDigest8(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum)[:8]
}

// verdictRetainedName keys the artifact on the EFFECT identity only: one
// delivered effect is one artifact, whoever claims to have reviewed it. The
// content check against an existing file with this name is what refuses a
// conflicting retry or a second reviewer claim over one effect; a different
// effect for the same candidate gets its own name.
func verdictRetainedName(sha, effectID string) string {
	sum := sha256.Sum256([]byte(effectID))
	return fmt.Sprintf("%s-verdict-%x.md", strings.ToLower(shortSHA(strings.TrimSpace(sha))), sum[:6])
}

// composeVerdictArtifact renders the canonical front-matter artifact. The
// body IS the delivered effect (signature included), so the retained evidence
// and the published consumable callback carry one identity.
func composeVerdictArtifact(rec VerdictRecoveryRecord, effect VerdictEffect) string {
	ledgerVerdict, lErr := LedgerVerdict(effect.VerdictToken)
	if lErr != nil {
		ledgerVerdict = "BLOCKED"
	}
	var b strings.Builder
	b.WriteString("sha: " + strings.TrimSpace(rec.CandidateSHA) + "\n")
	b.WriteString("branch: " + strings.TrimSpace(rec.Branch) + "\n")
	b.WriteString("task: " + reviewledger.CloseableCardRef(rec.Ref) + "\n")
	b.WriteString("reviewer: " + strings.TrimSpace(rec.Reviewer) + "\n")
	if fam := strings.TrimSpace(rec.ReviewerFamily); fam != "" {
		b.WriteString("reviewer-family: " + fam + "\n")
	}
	b.WriteString("builder-family: " + reviewledger.FamilyUnrecorded + "\n")
	b.WriteString("verdict: " + ledgerVerdict + "\n")
	b.WriteString("reviewed-base: " + strings.TrimSpace(rec.BaseSHA) + "\n")
	b.WriteString("reviewed-head: " + strings.TrimSpace(rec.CandidateSHA) + "\n")
	b.WriteString("---\n")
	b.WriteString(effect.Body + "\n")
	if v := strings.TrimSpace(rec.Verification); v != "" {
		b.WriteString("\n## Verification\n" + v + "\n")
	}
	return b.String()
}

// verificationDigestOf digests the verification record exactly the way
// admission will: the record is embedded under the one verification heading
// in the composed artifact, and the digest is over the section that parse
// extracts. Simulating that extraction here keeps the stated digest checkable
// without a second markdown parser.
func verificationDigestOf(verification string) string {
	if strings.TrimSpace(verification) == "" {
		return ""
	}
	a := Artifact{Body: "## Verification\n" + strings.TrimRight(verification, "\r\n") + "\n"}
	section := a.VerificationEvidence()
	if section == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(section))
	return fmt.Sprintf("%x", sum)[:32]
}
