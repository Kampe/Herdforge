package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
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
		return daemon.CompletionBinding{}, fmt.Errorf("no positive lease generation for %s (lifecycle must record the active lease)", ref)
	}
	return bind, nil
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
