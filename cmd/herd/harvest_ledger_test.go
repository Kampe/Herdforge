package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

type harvestLedgerFixture struct {
	repo, lane, candidate, canonical, decoy string
	binding                                 verifyLandedBinding
	evidence, decoyBefore                   []byte
}

func linkedHarvestLedgerFixture(t *testing.T) harvestLedgerFixture {
	t.Helper()
	repo, candidate, binding := publicEntryFixture(t)
	canonical := reviewledger.PathFor(repo)
	evidence, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	lane := filepath.Join(t.TempDir(), "invoker")
	surfaceGit(t, repo, "worktree", "add", "--detach", lane, "HEAD")
	decoy := reviewledger.PathFor(lane)
	if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real PASS for another existing object is stronger than an empty decoy:
	// neither admission nor omitted-candidate resolution may trust its identity.
	landed := surfaceGit(t, repo, "rev-parse", "HEAD")
	decoyBefore := bytes.ReplaceAll(evidence, []byte(candidate), []byte(landed))
	if bytes.Equal(evidence, decoyBefore) {
		t.Fatal("fixture evidence did not bind its candidate")
	}
	if err := os.WriteFile(decoy, decoyBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(lane)
	t.Setenv("HERD_REVIEW_LEDGER", "")
	t.Setenv("HERD_PROJECT_ROOT", "")
	t.Setenv("HERD_ROOT", lane)
	return harvestLedgerFixture{repo, lane, candidate, canonical, decoy, binding, evidence, decoyBefore}
}

func (f harvestLedgerFixture) assertDecoyUnchanged(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.decoy)
	if err != nil || !bytes.Equal(got, f.decoyBefore) {
		t.Fatalf("canonical harvest changed the caller's decoy ledger: %v", err)
	}
	if _, err := os.Stat(reviewledger.QueuePathFor(f.decoy)); !os.IsNotExist(err) {
		t.Fatalf("canonical harvest created or touched a caller-local queue: %v", err)
	}
}

func TestHarvestLandedUsesCanonicalLedgerFromLinkedWorktree(t *testing.T) {
	f := linkedHarvestLedgerFixture(t)
	if err := runHarvestVerifyLanded(pinProofBranch, f.binding); err != nil {
		if strings.Contains(err.Error(), "ledger_refused") {
			t.Fatalf("public landed entry lost canonical exact-candidate evidence: %v", err)
		}
		t.Fatalf("public landed entry failed outside ledger admission: %v", err)
	}
	assertReceiptAndDispositionAgree(t, f.lane)
	f.assertDecoyUnchanged(t)
}

func TestHarvestCanonicalLedgerSelectsAndPinsTheSameCandidate(t *testing.T) {
	f := linkedHarvestLedgerFixture(t)
	if _, err := harvestMergeVerdict(f.candidate, "", false); err != nil {
		t.Fatalf("harvest verdict lost canonical evidence: %v", err)
	}
	report, err := resolveHarvestCandidateWithReconstructionAt(f.lane, pinProofBranch, f.candidate, "", "")
	if err != nil || report.Pin.SHA != f.candidate {
		t.Fatalf("harvest selection lost exact canonical candidate: %+v, %v", report, err)
	}
	ctx, cancel := (&mergeadmit.Gate{ProofBudget: mergeadmit.ProofBudget{MaxCommands: 1}}).ProofContext()
	defer cancel()
	gate, err := buildMergeGateContext(ctx, f.binding.Ref, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	binding := f.binding
	binding.Candidate = ""
	binding.ledgerPath = gate.Ledger.Path
	if _, err := mergeadmit.BoundedGit(ctx, f.lane)("rev-parse", "HEAD"); !errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
		t.Fatalf("canonical discovery did not spend the existing allowance: %v", err)
	}
	// With the single discovery command already spent, both pins must use the
	// exact resolved ledger without performing another unbounded discovery.
	for name, resolve := range map[string]func() (string, error){
		"surface": func() (string, error) { return pinnedCandidateFor(ctx, binding) },
		"candidate": func() (string, error) {
			return resolveVerifyLandedCandidate(ctx, f.lane, pinProofBranch, binding)
		},
	} {
		got, err := resolve()
		if err != nil || got != f.candidate {
			t.Fatalf("%s pin disagrees with the resolved admission ledger: %q, %v", name, got, err)
		}
	}
	if err := runHarvestVerifyLanded(pinProofBranch, binding); err != nil {
		t.Fatalf("omitted candidate did not seal from canonical evidence: %v", err)
	}
	assertReceiptAndDispositionAgree(t, f.lane)
	f.assertDecoyUnchanged(t)
}

func TestHarvestCanonicalLedgerDoesNotBorrowDecoyAuthority(t *testing.T) {
	for _, failure := range []string{"absent", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			f := linkedHarvestLedgerFixture(t)
			// This time the decoy can authorize the exact candidate. Losing the
			// canonical ledger must still refuse rather than borrow that PASS.
			f.decoyBefore = f.evidence
			if err := os.WriteFile(f.decoy, f.evidence, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(f.canonical); err != nil {
				t.Fatal(err)
			}
			if failure == "unreadable" {
				if err := os.Mkdir(f.canonical, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := runHarvestVerifyLanded(pinProofBranch, f.binding); err == nil {
				t.Fatal("caller-local PASS authorized a missing or unreadable canonical ledger")
			}
			if receipt, disposition := publicEntryArtifacts(t, f.lane); receipt || disposition {
				t.Fatal("canonical ledger refusal produced completion artifacts")
			}
			f.assertDecoyUnchanged(t)
		})
	}
}

func TestHarvestCanonicalLedgerHonorsExplicitOverrides(t *testing.T) {
	f := linkedHarvestLedgerFixture(t)
	if err := os.WriteFile(f.decoy, f.evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.canonical); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(".herd", reviewledger.Leaf))
	ctx, cancel := cliMergeProofContext()
	defer cancel()
	got, err := resolveHarvestLedgerPath(ctx, ".")
	if err != nil || got != filepath.Join(".herd", reviewledger.Leaf) {
		t.Fatalf("explicit ledger override changed: %q, %v", got, err)
	}
	if err := runHarvestVerifyLanded(pinProofBranch, f.binding); err != nil {
		t.Fatalf("explicit exact-candidate override was not honored: %v", err)
	}
	assertReceiptAndDispositionAgree(t, f.lane)
	// Project-root override also keeps its established precedence over both
	// Git discovery and the conflicting lane root, even outside a repository.
	t.Setenv("HERD_REVIEW_LEDGER", "")
	t.Setenv("HERD_PROJECT_ROOT", f.repo)
	got, err = resolveHarvestLedgerPath(ctx, t.TempDir())
	if err != nil || got != f.canonical {
		t.Fatalf("explicit project root lost precedence over lane/discovery: %q, %v", got, err)
	}
}

func TestHarvestCanonicalDiscoveryRefusesErrorsAndStoppedContexts(t *testing.T) {
	f := linkedHarvestLedgerFixture(t)
	ctx, cancel := cliMergeProofContext()
	defer cancel()
	notRepo := t.TempDir()
	if ledger, err := openHarvestLedgerContext(ctx, notRepo); err == nil || ledger != nil {
		t.Fatalf("failed canonical discovery fell back to a local ledger: %+v, %v", ledger, err)
	}
	if _, err := os.Stat(filepath.Join(notRepo, ".herd")); !os.IsNotExist(err) {
		t.Fatalf("failed discovery created local authority files: %v", err)
	}
	for _, expired := range []bool{false, true} {
		stopped, stop := context.WithCancel(ctx)
		want := context.Canceled
		if expired {
			stop()
			stopped, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			stop()
		}
		_, err := buildMergeGateContext(stopped, f.binding.Ref, "", 0)
		stop()
		if !errors.Is(err, want) {
			t.Fatalf("canonical gate discovery escaped stopped context: %v, want %v", err, want)
		}
	}
	f.assertDecoyUnchanged(t)
}

func TestHarvestLandedCanonicalDiscoverySharesPublicAllowance(t *testing.T) {
	f := linkedHarvestLedgerFixture(t)
	previous := cliProofBudget
	t.Cleanup(func() { cliProofBudget = previous })
	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: 1}
	err := runHarvestVerifyLanded(pinProofBranch, f.binding)
	if !errors.Is(err, mergeadmit.ErrProofBudgetCommands) || !strings.Contains(err.Error(), "could not determine whether a carrier") {
		t.Fatalf("public entry did not charge discovery before carrier selection: %v", err)
	}
	if receipt, disposition := publicEntryArtifacts(t, f.lane); receipt || disposition {
		t.Fatal("canonical discovery exhaustion produced completion artifacts")
	}
	cliProofBudget = mergeadmit.ProofBudget{}
	if err := runHarvestVerifyLanded(pinProofBranch, f.binding); err != nil {
		t.Fatalf("the same canonical fixture cannot seal with its default allowance: %v", err)
	}
	assertReceiptAndDispositionAgree(t, f.lane)
	f.assertDecoyUnchanged(t)
}
