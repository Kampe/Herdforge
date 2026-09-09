package reviewingest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
		BranchReaches: func(branch, sha string) bool { return branch == fxBranch && sha == fxCand },
		LatestDeliveredVerdict: func(repo, ref, candidate string) (string, int64, bool, error) {
			return f.effect.EffectID, 587, true, nil
		},
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
		opts := f.opts()
		opts.BranchReaches = nil
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("recovery without branch-reach capability must refuse")
		}
		opts = f.opts()
		opts.LatestDeliveredVerdict = nil
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("recovery without bus-order capability must refuse")
		}
	})
	t.Run("requires reviewer family and refuses coordinator identity", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		f.record.ReviewerFamily = ""
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("recovery without reviewer family must refuse")
		}
		f = newRecoveryFixture(t, "APPROVED")
		f.record.Reviewer = "coordinator"
		if _, err := RecoverVerdictArtifact(f.root, f.record, f.opts()); err == nil {
			t.Fatal("a coordinator identity must never be recovered as reviewer")
		}
	})
	t.Run("refuses branch that does not reach the candidate", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
		opts := f.opts()
		opts.BranchReaches = func(branch, sha string) bool { return false }
		if _, err := RecoverVerdictArtifact(f.root, f.record, opts); err == nil {
			t.Fatal("an unreachable branch must be refused")
		}
	})
	t.Run("refuses superseded effect", func(t *testing.T) {
		f := newRecoveryFixture(t, "APPROVED")
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
