package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/dispatch"
)

func gitC(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func gitIsAncestor(dir, ancestor, rev string) bool {
	ancestor = strings.TrimSpace(ancestor)
	rev = strings.TrimSpace(rev)
	if ancestor == "" || rev == "" {
		return false
	}
	return exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", ancestor, rev).Run() == nil
}

func sameIssuanceSHA(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	al, bl := strings.ToLower(a), strings.ToLower(b)
	if len(al) < 12 || len(bl) < 12 {
		return false
	}
	return strings.HasPrefix(al, bl) || strings.HasPrefix(bl, al)
}

// verifierIssuanceBase picks a base that is an ancestor of the candidate.
// origin/main after later merges is not that base. Prefer an authenticated
// builder base when present; otherwise use merge-base with origin/main.
// An explicit --base must itself be ancestral and must not conflict with a
// verified authenticated base.
func verifierIssuanceBase(dir, originMain, candidate, authenticatedBase, explicitBase string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", fmt.Errorf("verifier issuance requires a candidate SHA")
	}
	explicitBase = strings.TrimSpace(explicitBase)
	authenticatedBase = strings.TrimSpace(authenticatedBase)
	originMain = strings.TrimSpace(originMain)

	if explicitBase != "" && authenticatedBase != "" && !sameIssuanceSHA(explicitBase, authenticatedBase) {
		return "", fmt.Errorf("verifier issuance: --base %s conflicts with authenticated builder base %s", explicitBase, authenticatedBase)
	}

	pick := explicitBase
	if pick == "" {
		pick = authenticatedBase
	}
	if pick == "" {
		if originMain == "" {
			return "", fmt.Errorf("verifier issuance has no readable origin/main to compute a candidate-relative base")
		}
		mb, err := gitC(dir, "merge-base", candidate, originMain)
		if err != nil || mb == "" {
			return "", fmt.Errorf("verifier issuance: no merge-base of candidate %s and origin/main %s", candidate, originMain)
		}
		pick = mb
	}
	if !gitIsAncestor(dir, pick, candidate) {
		return "", fmt.Errorf("verifier issuance base %s is not an ancestor of candidate %s", pick, candidate)
	}
	return pick, nil
}

// authenticatedBuilderBase returns a verified worker/recovery BaseSHA.
// Missing TASK-CONTEXT is absence (empty, nil) and issuance may infer a
// merge-base. A present context that fails to read, authenticate, match the
// candidate, or carry a builder role is refused so issuance cannot silently
// sign an inferred base over invalid identity.
func authenticatedBuilderBase(root, targetDir, candidate string) (string, error) {
	prior, err := dispatch.ReadTaskContext(targetDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("verifier issuance: authenticated builder context: %w", err)
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return "", fmt.Errorf("verifier issuance: cannot authenticate builder context: %w", err)
	}
	if err := verifier.Verify(prior); err != nil {
		return "", fmt.Errorf("verifier issuance: builder context failed authentication: %w", err)
	}
	if got := strings.TrimSpace(prior.CandidateSHA); got != "" && !sameIssuanceSHA(got, candidate) {
		return "", fmt.Errorf("verifier issuance: authenticated builder context is for candidate %s, not %s", got, strings.TrimSpace(candidate))
	}
	role := strings.ToLower(strings.TrimSpace(prior.Role))
	if role != dispatch.RoleWorker && role != dispatch.RoleRecovery {
		return "", fmt.Errorf("verifier issuance: authenticated builder context role %q is not worker or recovery", prior.Role)
	}
	base := strings.TrimSpace(prior.BaseSHA)
	if base == "" {
		return "", fmt.Errorf("verifier issuance: authenticated builder context has empty BaseSHA")
	}
	return base, nil
}
