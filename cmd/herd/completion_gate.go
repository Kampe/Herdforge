package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

// FAC-144: production completion → review admission wiring for forge and
// review --spawn. CheckCompletion is never review authority.

const (
	defaultReceiptDir   = ".herd/verification-receipts"
	defaultLifecycleDB  = ".herd/lifecycle.db"
	verificationProfile = "config-verification"
)

func openCompletionGate(cfg *config.Config) (*daemon.CompletionGate, *lifecycle.Machine, error) {
	testCmd := ""
	if cfg != nil {
		testCmd = strings.TrimSpace(cfg.Verification.TestCommand)
	}
	if testCmd == "" {
		return nil, nil, fmt.Errorf("verification.test_command is required for completion admission")
	}
	v := verifier.NewVerifier(testCmd)
	machine, err := lifecycle.NewMachine(defaultLifecycleDB)
	if err != nil {
		return nil, nil, fmt.Errorf("open lifecycle machine: %w", err)
	}
	gate, err := daemon.NewCompletionGate(v, defaultReceiptDir, machine)
	if err != nil {
		_ = machine.Close()
		return nil, nil, err
	}
	return gate, machine, nil
}

func worktreeHeadSHA(wt string) (string, error) {
	cmd := exec.Command("git", "-C", wt, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve candidate SHA: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 {
		return "", fmt.Errorf("candidate SHA is not exact (got %q)", sha)
	}
	return sha, nil
}

func bindingForWorktree(cfg *config.Config, machine *lifecycle.Machine, ref, wt string) (daemon.CompletionBinding, error) {
	root, err := canonicalHerdRoot()
	if err != nil {
		return daemon.CompletionBinding{}, err
	}
	return bindingForWorktreeAtRoot(cfg, machine, ref, wt, root)
}

func bindingForWorktreeAtRoot(cfg *config.Config, machine *lifecycle.Machine, ref, wt, root string) (daemon.CompletionBinding, error) {
	sha, err := worktreeHeadSHA(wt)
	if err != nil {
		return daemon.CompletionBinding{}, err
	}
	bind := daemon.CompletionBinding{
		TaskRef:             ref,
		Repo:                "herdforge",
		CandidateSHA:        sha,
		WorktreeDir:         wt,
		VerificationProfile: verificationProfile,
		Branch:              "herd/" + strings.ToLower(ref),
	}
	if cfg != nil && cfg.Verification.PreflightCommand != "" {
		bind.VerificationProfile = verificationProfile + "+preflight"
	}
	if machine != nil {
		if ts, err := machine.EventStore().CurrentState(ref); err == nil && ts != nil {
			if ts.LeaseGeneration > 0 {
				bind.LeaseGeneration = ts.LeaseGeneration
			}
			if ts.Branch != "" {
				bind.Branch = ts.Branch
			}
			if ts.Repo != "" {
				bind.Repo = ts.Repo
			}
			if ts.CandidateSHA != "" && ts.CandidateSHA != sha {
				// Live HEAD moved past lifecycle's recorded candidate —
				// still bind to live HEAD; prior digests will not admit.
			}
		}
	}
	if bind.LeaseGeneration <= 0 {
		// Forge's early claim path predated lifecycle projection. A signed,
		// unexpired launch receipt is the only safe compatibility source for
		// those claims; unsigned or conflicting receipts never get promoted.
		tc, receiptErr := signedLaunchReceiptForReview(root, ref, wt)
		if receiptErr == nil {
			bind.LeaseGeneration = tc.LeaseGeneration
			if tc.Branch != "" {
				bind.Branch = tc.Branch
			}
			if tc.Repository != "" {
				bind.Repo = tc.Repository
			}
		} else {
			return daemon.CompletionBinding{}, fmt.Errorf("no positive lease generation for %s (lifecycle missing and signed receipt fallback refused: %v)", ref, receiptErr)
		}
	}
	return bind, nil
}

// signedLaunchReceiptForReview authenticates the durable forge launch receipt
// before using its lease generation as a legacy lifecycle fallback. The
// canonical receipt is authoritative; a worktree copy is only considered when
// the canonical store has no entry (for example, an interrupted legacy write).
func signedLaunchReceiptForReview(root, ref, wt string) (dispatch.TaskContext, error) {
	var (
		tc  dispatch.TaskContext
		err error
	)
	// The candidate worktree's signed worker context is the authority for
	// completion evidence. A later reviewer launch writes its own canonical
	// receipt for the same task, often with a newer review-lease generation;
	// selecting that receipt here makes the worker's verification receipts
	// appear stale and blocks an otherwise valid re-review. Prefer the local
	// context whenever the file exists, then fall back to the canonical store
	// only for legacy worktrees that predate TASK-CONTEXT.json.
	localPath := filepath.Join(wt, dispatch.TaskContextFile)
	if _, statErr := os.Stat(localPath); statErr == nil {
		tc, err = dispatch.ReadTaskContext(wt)
		if err != nil {
			return tc, err
		}
		if tc.TaskRef != ref {
			return tc, fmt.Errorf("receipt task ref %q does not match %q", tc.TaskRef, ref)
		}
		if tc.Role != dispatch.RoleWorker && tc.Role != dispatch.RoleVerifier {
			return tc, fmt.Errorf("worktree receipt role %q cannot authorize review admission", tc.Role)
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		tc, err = dispatch.LoadCanonicalReceipt(root, ref)
		if err != nil {
			return tc, err
		}
	} else {
		return tc, fmt.Errorf("stat task context: %w", statErr)
	}
	if !tc.ExpiresAt.After(time.Now()) {
		return tc, fmt.Errorf("receipt expired at %s", tc.ExpiresAt.UTC().Format(time.RFC3339))
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return tc, err
	}
	if err := verifier.Verify(tc); err != nil {
		return tc, err
	}
	if tc.LeaseGeneration <= 0 {
		return tc, fmt.Errorf("receipt has no positive lease generation")
	}
	return tc, nil
}

// recoverVerificationDigest restores the review admission handle for a
// legacy completion receipt. Old forge runs could persist a valid PASS body
// without its redundant digest field or under a non-digest filename. We scan
// only the repo-local receipt store, bind every field to this exact task,
// generation, and candidate, recompute the self-authenticating digest, then
// persist the canonical filename before returning it to ReceiptAdmission.
func recoverVerificationDigest(ctx context.Context, root, ref, wt, candidateSHA string, leaseGeneration int64) (string, error) {
	dir := filepath.Join(root, defaultReceiptDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no legacy verification receipt found for %s", ref)
		}
		return "", fmt.Errorf("verification receipt store unavailable: %w", err)
	}
	wantGeneration := fmt.Sprintf("%d", leaseGeneration)
	var rejectedOutcome verifier.Outcome
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return "", fmt.Errorf("read legacy verification receipt: %w", readErr)
		}
		var receipt verifier.Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return "", fmt.Errorf("decode legacy verification receipt: %w", err)
		}
		if receipt.TaskRef != ref || receipt.CandidateSHA != candidateSHA || receipt.LeaseGeneration != wantGeneration {
			continue
		}
		if receipt.Outcome != verifier.OutcomePASS {
			// The store is append-only and may contain an earlier FAIL/BLOCKED
			// attempt for the same candidate. Keep scanning for a later PASS;
			// directory order is not a recency or precedence contract.
			rejectedOutcome = receipt.Outcome
			continue
		}
		if receipt.Digest == "" {
			receipt.Digest = receipt.ComputeDigest()
		} else if err := receipt.ValidateDigest(); err != nil {
			return "", fmt.Errorf("matching verification receipt digest invalid: %w", err)
		}
		if err := receipt.ValidateReceipt(ctx, wt); err != nil {
			return "", fmt.Errorf("matching verification receipt is no longer current: %w", err)
		}
		store, err := verifier.NewFileReceiptStore(dir)
		if err != nil {
			return "", err
		}
		if err := store.Persist(ctx, receipt); err != nil {
			return "", fmt.Errorf("restamp verification receipt: %w", err)
		}
		return receipt.Digest, nil
	}
	if rejectedOutcome != "" {
		return "", fmt.Errorf("matching verification receipt outcome is %s, not PASS", rejectedOutcome)
	}
	return "", fmt.Errorf("no PASS verification receipt for %s candidate %s generation %d", ref, shortSHA(candidateSHA), leaseGeneration)
}

// verifyWorktreeForReview runs the FAC-144 gate and reports whether the
// candidate is review-ready. Errors are hard failures (not soft skips).
func verifyWorktreeForReview(ctx context.Context, cfg *config.Config, ref, wt string) (ready bool, digest string, reason string, err error) {
	gate, machine, err := openCompletionGate(cfg)
	if err != nil {
		return false, "", "", err
	}
	defer machine.Close()

	// Optional preflight as a separate command before the configured test
	// command. Failures are FAIL (repair), not silent skips.
	if cfg != nil && strings.TrimSpace(cfg.Verification.PreflightCommand) != "" {
		pre := verifier.NewVerifier(cfg.Verification.PreflightCommand)
		store, storeErr := verifier.NewFileReceiptStore(defaultReceiptDir)
		if storeErr != nil {
			return false, "", "", storeErr
		}
		bind, berr := bindingForWorktree(cfg, machine, ref, wt)
		if berr != nil {
			return false, "", berr.Error(), berr
		}
		req := verifier.VerificationRequest{
			TaskRef:           bind.TaskRef,
			LeaseGeneration:   fmt.Sprintf("%d", bind.LeaseGeneration),
			CandidateSHA:      bind.CandidateSHA,
			EnvironmentPolicy: verifier.EnvironmentPolicyInherited,
		}
		preReceipt, perr := pre.VerifyAndPersist(ctx, wt, req, store)
		if perr != nil {
			return false, "", "", perr
		}
		if preReceipt.Outcome != verifier.OutcomePASS {
			return false, preReceipt.Digest, fmt.Sprintf("preflight %s", preReceipt.Outcome), nil
		}
	}

	bind, err := bindingForWorktree(cfg, machine, ref, wt)
	if err != nil {
		return false, "", err.Error(), err
	}
	d, err := gate.HandleCompletion(ctx, bind)
	if d != nil {
		digest = d.Digest
		reason = d.Reason
	}
	if err != nil {
		// FAIL/BLOCKED are expected non-review outcomes.
		if d != nil && (d.Outcome == verifier.OutcomeFAIL || d.Outcome == verifier.OutcomeBLOCKED) {
			return false, digest, reason, nil
		}
		return false, digest, reason, err
	}
	return d != nil && d.ReviewReady, digest, reason, nil
}

// admitWorktreeForReview re-checks RequireCurrentPassing before review spawn.
func admitWorktreeForReview(ctx context.Context, cfg *config.Config, ref, wt, digest string) error {
	gate, machine, err := openCompletionGate(cfg)
	if err != nil {
		return err
	}
	defer machine.Close()
	bind, err := bindingForWorktree(cfg, machine, ref, wt)
	if err != nil {
		return err
	}
	if digest == "" {
		// Recover digest from lifecycle evidence if the caller did not
		// carry it (e.g. restart between Signals and Review).
		if events, eerr := machine.EventStore().Events(ref); eerr == nil {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].CandidateSHA == bind.CandidateSHA && strings.HasPrefix(events[i].EvidenceDigest, "sha256:") {
					digest = events[i].EvidenceDigest
					break
				}
			}
		}
	}
	if digest == "" {
		root, rootErr := canonicalHerdRoot()
		if rootErr != nil {
			return rootErr
		}
		digest, err = recoverVerificationDigest(ctx, root, ref, wt, bind.CandidateSHA, bind.LeaseGeneration)
		if err != nil {
			return fmt.Errorf("missing receipt digest and legacy recovery refused: %w", err)
		}
	}
	if _, err := gate.AdmitReview(ctx, bind, digest); err != nil {
		return err
	}
	return nil
}

func worktreePathForRef(ref string) string {
	return filepath.Join(".herd", "worktrees", strings.ToLower(ref))
}

func worktreeExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// useHarnessHooksFromWorktree supplies the repository-declared hook policy
// when a coordinator is reviewing a candidate before that candidate has
// landed on the coordinator's checkout. The candidate worktree is already
// authenticated by review admission; using its .herd/harness-hooks.json
// keeps native Herdr launches independent of an operator-set environment
// variable while preserving an explicit override when one is present.
func useHarnessHooksFromWorktree(wt string) func() {
	if strings.TrimSpace(os.Getenv("HERD_HARNESS_HOOKS_FILE")) != "" {
		return func() {}
	}
	path := filepath.Join(wt, ".herd", "harness-hooks.json")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return func() {}
	}
	previous, hadPrevious := os.LookupEnv("HERD_HARNESS_HOOKS_FILE")
	if err := os.Setenv("HERD_HARNESS_HOOKS_FILE", path); err != nil {
		return func() {}
	}
	return func() {
		if hadPrevious {
			_ = os.Setenv("HERD_HARNESS_HOOKS_FILE", previous)
		} else {
			_ = os.Unsetenv("HERD_HARNESS_HOOKS_FILE")
		}
	}
}
