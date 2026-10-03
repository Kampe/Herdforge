package reviewingest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recoveryFixture builds a signed delivered effect and a matching record, the
// way the broker composes them (FAC-145): the signature preimage is
// "herd-verdict-effect:" + effectID + "\n" + line.
type recoveryFixture struct {
	key    ed25519.PrivateKey
	root   string
	effect VerdictEffect
	record VerdictRecoveryRecord
}

const (
	fxTask   = "FAC-737"
	fxCand   = "1111111111111111111111111111111111111111"
	fxBase   = "2222222222222222222222222222222222222222"
	fxLease  = "claim:318"
	fxBranch = "harvest/fac-737-887"
)

func newRecoveryFixture(t *testing.T, token string) *recoveryFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	effectID := "verdict-delivered:herdforge:" + fxTask + ":" + fxCand + ":gen1:" + fxLease + ":" + token
	line := "REVIEW VERDICT " + fxTask + ": " + token +
		" candidate=" + fxCand + " base=" + fxBase + " lease=" + fxLease +
		" lease_gen=1 reviewer-bound (FAC-145)"
	sig := ed25519.Sign(key, []byte("herd-verdict-effect:"+effectID+"\n"+line))
	body := line + " [effect " + effectID + " sig=" + hex.EncodeToString(sig) + "]"
	f := &recoveryFixture{key: key, root: t.TempDir()}
	f.effect = VerdictEffect{EffectID: effectID, Line: line, Body: body}
	f.record = VerdictRecoveryRecord{
		Repo: "herdforge", Ref: fxTask, CandidateSHA: fxCand, BaseSHA: fxBase,
		Branch: fxBranch, LeaseID: fxLease, LeaseGeneration: 1, Verdict: token,
		Reviewer: "review-fac-737-111111111111", ReviewerFamily: "anthropic",
		CanonicalBody: body,
	}
	return f
}

func (f *recoveryFixture) opts() VerdictRecoveryOptions {
	pub := f.key.Public().(ed25519.PublicKey)
	return VerdictRecoveryOptions{
		VerifyEffect: func(line, effectID, sigHex string) error {
			sig, err := hex.DecodeString(sigHex)
			if err != nil {
				return err
			}
			if !ed25519.Verify(pub, []byte("herd-verdict-effect:"+effectID+"\n"+line), sig) {
				return os.ErrInvalid
			}
			return nil
		},
		VerifyRecord: func(data []byte, sigHex string) error {
			sig, err := hex.DecodeString(sigHex)
			if err != nil {
				return err
			}
			if !ed25519.Verify(pub, data, sig) {
				return os.ErrInvalid
			}
			return nil
		},
		BranchReaches: func(branch, sha string) bool { return branch == fxBranch && sha == fxCand },
		LatestDeliveredVerdict: func(repo, ref, candidate string) (string, int64, bool, error) {
			return f.effect.EffectID, 587, true, nil
		},
	}
}

// signRecord signs the record in its CURRENT state — call after any
// legitimate mutation and before recovery, exactly as the coordinator CLI
// would. Fixture records are well-formed by construction, so a signing
// failure here is a fixture bug, not a test outcome.
func (f *recoveryFixture) signRecord() {
	if _, err := SignRecoveryRecord(&f.record, func(data []byte) (string, error) {
		return hex.EncodeToString(ed25519.Sign(f.key, data)), nil
	}); err != nil {
		panic("fixture signing failed: " + err.Error())
	}
}

func TestRetainVerdictArtifact_WritesCanonicalInboxArtifactOnce(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	rel, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if !IsInboxPath(rel) {
		t.Fatalf("retained path %s is not under the canonical inbox", rel)
	}
	dst := filepath.Join(f.root, filepath.FromSlash(rel))
	if !strings.HasPrefix(filepath.Base(rel), "111111111111-verdict-") {
		t.Fatalf("artifact name %s is not content-addressed on the effect", rel)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("retained artifact mode %v, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"sha: " + fxCand, "branch: " + fxBranch, "task: " + fxTask,
		"reviewer: " + f.record.Reviewer, "builder-family: unrecorded",
		"verdict: PASS", "reviewed-base: " + fxBase, "reviewed-head: " + fxCand,
		f.effect.Body,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("retained artifact missing %q:\n%s", want, text)
		}
	}

	// Idempotent: the same effect retains exactly once with the same path.
	rel2, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err != nil || rel2 != rel {
		t.Fatalf("re-retain: rel=%s err=%v (want idempotent %s)", rel2, err, rel)
	}

	// Conflicting content under the same effect identity is refused, and the
	// original evidence is untouched.
	conflicting := f.record
	conflicting.Reviewer = "review-imposter-222222222222"
	if _, err := RetainVerdictArtifact(f.root, conflicting, f.opts()); err == nil {
		t.Fatal("conflicting re-retain must be refused")
	}
	after, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(after), f.record.Reviewer) {
		t.Fatalf("original retained artifact was rewritten or vanished: %v", err)
	}
}

func TestRetainVerdictArtifact_RefusesUnsignedEffect(t *testing.T) {
	f := newRecoveryFixture(t, "REJECTED")
	opts := f.opts()
	opts.VerifyEffect = func(line, effectID, sigHex string) error {
		return os.ErrInvalid
	}
	if _, err := RetainVerdictArtifact(f.root, f.record, opts); err == nil {
		t.Fatal("unsigned effect must be refused")
	}
	if _, err := os.ReadDir(filepath.Join(f.root, InboxRel)); !os.IsNotExist(err) {
		t.Fatalf("a refused verdict must retain nothing: %v", err)
	}
}

func TestRetainVerdictArtifact_RefusesIntentOnlyRecord(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	// FAC-739 gen1 seq592 shape: an intent record carries the intent line but
	// no delivered effect trailer or signature.
	f.record.CanonicalBody = "verdict intent (undelivered): " + f.effect.Line
	if _, err := RetainVerdictArtifact(f.root, f.record, f.opts()); err == nil {
		t.Fatal("intent-only evidence must never be retained")
	}
}

func TestRetainVerdictArtifact_CrossCheckMatrix(t *testing.T) {
	cases := map[string]func(r *VerdictRecoveryRecord){
		"wrong sha":   func(r *VerdictRecoveryRecord) { r.CandidateSHA = "3333333333333333333333333333333333333333" },
		"wrong base":  func(r *VerdictRecoveryRecord) { r.BaseSHA = "4444444444444444444444444444444444444444" },
		"wrong lease": func(r *VerdictRecoveryRecord) { r.LeaseID = "claim:999" },
		"wrong gen":   func(r *VerdictRecoveryRecord) { r.LeaseGeneration = 2 },
		"wrong task":  func(r *VerdictRecoveryRecord) { r.Ref = "FAC-738" },
		"wrong verdict": func(r *VerdictRecoveryRecord) {
			r.Verdict = "REJECTED"
		},
		"tampered detail": func(r *VerdictRecoveryRecord) { r.Detail = "retrofitted detail" },
	}
	for name, mutate := range cases {
		f := newRecoveryFixture(t, "APPROVED")
		mutate(&f.record)
		if _, err := RetainVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatalf("%s: mismatched record must be refused", name)
		}
	}
}

func TestRetainVerdictArtifact_VerificationDigestIsNeverSynthesized(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	f.record.Verification = "go test -count=1 ./pkg/reviewingest — PASS\nrainbow check: ok"
	f.record.VerificationDigest = "deadbeef"
	if _, err := RetainVerdictArtifact(f.root, f.record, f.opts()); err == nil {
		t.Fatal("a stated digest that does not match the record must be refused")
	}
	a := Artifact{Body: "## Verification\n" + f.record.Verification + "\n"}
	section := a.VerificationEvidence()
	if section == "" {
		t.Fatal("verification record must extract to a non-empty section")
	}
	// The correct digest is the one admission will compute over the same
	// section; a changed command must produce a different digest.
	f.record.VerificationDigest = verificationDigestOf(f.record.Verification)
	rel, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err != nil {
		t.Fatalf("retain with correct digest: %v", err)
	}
	text, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "## Verification") {
		t.Fatalf("verification record not embedded:\n%s", text)
	}
	f.record.Verification = strings.Replace(f.record.Verification, "PASS", "FAIL", 1)
	if verificationDigestOf(f.record.Verification) == f.record.VerificationDigest {
		t.Fatal("digest must bind verification content")
	}
}

func TestRecoverVerdictArtifact_Gates(t *testing.T) {
	t.Run("requires capability", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.signRecord()
		opts := f.opts()
		opts.BranchReaches = nil
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("recovery without branch-reach capability must refuse")
		}
		f.signRecord()
		opts = f.opts()
		opts.LatestDeliveredVerdict = nil
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("recovery without bus-order capability must refuse")
		}
	})
	t.Run("requires reviewer family and refuses coordinator identity", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.record.ReviewerFamily = ""
		f.signRecord()
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("recovery without reviewer family must refuse")
		}
		f = newRecoveryFixture(t, "APPROVED")
		f.record.Reviewer = "coordinator"
		f.signRecord()
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("a coordinator identity must never be recovered as reviewer")
		}
	})
	t.Run("refuses branch that does not reach the candidate", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.signRecord()
		opts := f.opts()
		opts.BranchReaches = func(branch, sha string) bool { return false }
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("an unreachable branch must be refused")
		}
	})
	t.Run("refuses superseded effect", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.signRecord()
		opts := f.opts()
		opts.LatestDeliveredVerdict = func(repo, ref, candidate string) (string, int64, bool, error) {
			return "verdict-delivered:herdforge:" + fxTask + ":" + fxCand + ":gen1:" + fxLease + ":REJECTED", 590, true, nil
		}
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("a superseded effect must be refused")
		}
	})
	t.Run("refuses stale bus binding and duplicate-conflicting sequence", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.record.BusSequence = 587
		f.signRecord()
		opts := f.opts()
		opts.LatestDeliveredVerdict = func(repo, ref, candidate string) (string, int64, bool, error) {
			return "", 0, false, nil
		}
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("a bound bus callback that no longer exists must be refused as stale")
		}
		opts.LatestDeliveredVerdict = func(repo, ref, candidate string) (string, int64, bool, error) {
			return f.effect.EffectID, 588, true, nil
		}
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("a duplicate-conflicting bus sequence must be refused")
		}
	})
	t.Run("recovers the effective effect exactly once", func(t *testing.T) {
		f := newRecoveryFixture(t, "REJECTED")
		f.signRecord()
		rel, err := RecoverVerdictArtifact(f.root, f.record, f.opts())
		if err != nil {
			t.Fatalf("recover REJECTED: %v", err)
		}
		text, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(text), "verdict: FAIL") {
			t.Fatalf("REJECTED must map to the ledger FAIL verdict:\n%s", text)
		}
		if rel2, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err != nil || rel2 != rel {
			t.Fatalf("recovery must be idempotent: %s %v", rel2, err)
		}
	})
}

// TestRecoverVerdictArtifact_RefusesUnboundMetadata is the FAC-740 review
// finding 2 control: the signed provider effect covers NO reviewer, family,
// or verification field, so recovery must refuse any record whose metadata is
// not bound by a valid coordinator record signature. Against the shipped
// candidate (which checked only that a supplied digest matched a recomputed
// digest of supplied text) the tampered variants RECOVER — these assertions
// fail there.
func TestRecoverVerdictArtifact_RefusesUnboundMetadata(t *testing.T) {
	t.Run("refuses an unsigned record", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("an unsigned recovery record must be refused — metadata without signed authority is not independent-review evidence")
		}
	})
	t.Run("refuses a tampered reviewer or family", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.signRecord()
		f.record.Reviewer = "review-impersonated-999999999999"
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("a record whose reviewer was changed after signing must be refused")
		}
		f2 := newRecoveryFixture(t, "APPROVED")
		f2.signRecord()
		f2.record.ReviewerFamily = "openai"
		if _, err := RecoverVerdictArtifact(f2.root, f2.record, f2.opts()); err == nil {
			t.Fatal("a record whose reviewer family was changed after signing must be refused")
		}
	})
	t.Run("refuses tampered verification evidence", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.record.Verification = "go test ./... \nexit 0"
		f.record.VerificationDigest = verificationDigestOf(f.record.Verification)
		f.signRecord()
		f.record.Verification = "go test ./... \nexit 0 (self-authored rewrite)"
		// The record signature covers the stated digest, so rewritten
		// verification text cannot be smuggled in with a valid signature
		// over a different digest.
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("verification evidence rewritten after signing must be refused")
		}
	})
	t.Run("recovers a signed record whose metadata is bound", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.signRecord()
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err != nil {
			t.Fatalf("a coordinator-signed record must recover: %v", err)
		}
	})
}

// TestPublishRetainedArtifact_NeverReplacesExistingEvidence is the FAC-740
// review finding 3 control. os.Rename silently REPLACES the destination on
// POSIX, so the shipped candidate's check-then-rename publish let two
// concurrent recoveries of the same artifact name each report success while
// the last rename replaced the first evidence. With the atomic no-replace
// link the second publisher must observe the existing bytes and refuse.
func TestPublishRetainedArtifact_NeverReplacesExistingEvidence(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	dst := filepath.Join(f.root, ".herd", "review", "inbox", "collision-target.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	first := []byte("coordinator-reviewed evidence v1\n")
	if err := publishRetainedArtifact(dst, first); err != nil {
		t.Fatalf("first publish must succeed: %v", err)
	}
	if err := publishRetainedArtifact(dst, []byte("conflicting rewrite v2\n")); !errors.Is(err, ErrRetainedCollision) {
		t.Fatalf("a conflicting publish must collide, got %v", err)
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, first) {
		t.Fatalf("existing evidence must survive a colliding publish byte for byte, got %q", onDisk)
	}
	// Same content is an idempotent success.
	if err := publishRetainedArtifact(dst, first); err != nil {
		t.Fatalf("same-content publish must converge: %v", err)
	}
}

// TestPublishRetainedArtifact_ConcurrentSameContentConverges exercises the
// race itself: N writers of the SAME artifact name with the SAME content must
// all succeed and leave exactly one file; N writers with DIFFERENT content
// must leave exactly the first publisher's bytes and no torn mix.
func TestPublishRetainedArtifact_ConcurrentSameContentConverges(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	dir := filepath.Join(f.root, ".herd", "review", "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "concurrent-target.md")
	content := []byte("the effect body, identical for every writer\n")
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = publishRetainedArtifact(dst, content) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("same-content writer %d must converge: %v", i, err)
		}
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Fatalf("converged content must be byte-identical, got %q", onDisk)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("convergence must leave exactly one artifact, got %d", len(entries))
	}
}

func TestLedgerVerdict_TokenMapping(t *testing.T) {
	if v, err := LedgerVerdict("APPROVED"); err != nil || v != "PASS" {
		t.Fatalf("APPROVED -> %q %v", v, err)
	}
	if v, err := LedgerVerdict("REJECTED"); err != nil || v != "FAIL" {
		t.Fatalf("REJECTED -> %q %v", v, err)
	}
	if _, err := LedgerVerdict("CHANGES_REQUESTED"); err == nil {
		t.Fatal("non-broker tokens must be refused")
	}
}

func TestParseVerdictEffect_RejectsMalformedEvidence(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	cases := map[string]string{
		"no trailer":      f.effect.Line,
		"unsigned":        f.effect.Line + " [effect " + f.effect.EffectID + " sig=]",
		"intent prefix":   "verdict-intent:" + f.effect.EffectID,
		"non-hex sig":     f.effect.Line + " [effect " + f.effect.EffectID + " sig=zz]",
		"foreign effect":  f.effect.Line + " [effect some-other-id sig=ab]",
		"bad token":       strings.Replace(f.effect.Line, "APPROVED", "LGTM", 1) + " [effect " + f.effect.EffectID + " sig=ab]",
		"short candidate": strings.Replace(f.effect.Line, fxCand, "abc", 1) + " [effect " + f.effect.EffectID + " sig=ab]",
	}
	for name, body := range cases {
		if _, err := ParseVerdictEffect(body); err == nil {
			t.Fatalf("%s: must be refused", name)
		}
	}
	if _, err := ParseVerdictEffect(f.effect.Body); err != nil {
		t.Fatalf("the exact delivered body must parse: %v", err)
	}
}

// TestRetainVerdictArtifact_RefusesRepoIdentityMismatch binds the record's
// repository to the SIGNED effect identity: a coordinator-signed record may
// never re-use a valid effect from another repository — that would attribute
// review authority across repository boundaries (R3 round 5, finding 1).
func TestRetainVerdictArtifact_RefusesRepoIdentityMismatch(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	f.record.Repo = "some-other-repo"
	f.signRecord()
	_, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err == nil {
		t.Fatal("a record whose repository differs from the signed effect identity must be refused")
	}
	if !strings.Contains(err.Error(), "does not match record repository") {
		t.Fatalf("refusal must name the repository binding: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, InboxRel)); len(entries) != 0 {
		t.Fatalf("a refused record must publish nothing, got %d artifacts", len(entries))
	}
}

// TestRetainVerdictArtifact_DigestPrefixCollisionIsNotIdempotent proves the
// idempotence check is EXACT BYTES: a different artifact under the same effect
// name whose content merely shares the old 8-hex digest prefix must be
// REFUSED, never reported as an idempotent success (R3 round 5, finding 2).
// A 32-bit prefix collision is constructed directly (birthday over the
// verification suffix), which is exactly the class the digest-prefix check
// let through.
func TestRetainVerdictArtifact_DigestPrefixCollisionIsNotIdempotent(t *testing.T) {
	f := newRecoveryFixture(t, "APPROVED")
	// Compose with the effect exactly as production parses it from the
	// canonical body — never with fixture-side state.
	effect, perr := ParseVerdictEffect(f.record.CanonicalBody)
	if perr != nil {
		t.Fatal(perr)
	}
	digest8 := func(text string) string {
		sum := sha256.Sum256([]byte(text))
		return fmt.Sprintf("%x", sum)[:8]
	}
	// Birthday-search two verification suffixes whose composed artifacts
	// collide on the first 32 bits.
	seen := map[string]string{}
	var verA, verB string
	for n := 0; ; n++ {
		v := fmt.Sprintf("reviewer ran the verifier probe %d", n)
		f.record.Verification = v
		f.record.VerificationDigest = verificationDigestOf(v)
		d := digest8(composeVerdictArtifact(f.record, effect))
		if prev, ok := seen[d]; ok {
			verA, verB = prev, v
			break
		}
		seen[d] = v
		if n > 1<<20 {
			t.Fatal("birthday search exceeded budget")
		}
	}

	mk := func(ver string) VerdictRecoveryRecord {
		r := f.record
		r.Verification = ver
		r.VerificationDigest = verificationDigestOf(ver)
		return r
	}
	recA, recB := mk(verA), mk(verB)
	if digest8(composeVerdictArtifact(recA, effect)) != digest8(composeVerdictArtifact(recB, effect)) ||
		composeVerdictArtifact(recA, effect) == composeVerdictArtifact(recB, effect) {
		t.Fatal("fixture must produce distinct contents sharing the digest prefix")
	}

	f.record = recA
	f.signRecord()
	relA, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err != nil {
		t.Fatalf("first retain: %v", err)
	}
	f.record = recB
	f.signRecord()
	relB, err := RetainVerdictArtifact(f.root, f.record, f.opts())
	if err == nil {
		t.Fatalf("a 32-bit digest-prefix collision must NOT be an idempotent success: both retained at %s and %s", relA, relB)
	}
	if !strings.Contains(err.Error(), "exists with different content") {
		t.Fatalf("refusal must name the content conflict: %v", err)
	}
	if relB == relA {
		t.Fatal("collision must not report the artifact path as success")
	}
	existing, readErr := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(relA)))
	if readErr != nil || string(existing) != composeVerdictArtifact(recA, effect) {
		t.Fatalf("first-published evidence must survive byte-for-byte: %v", readErr)
	}
}
