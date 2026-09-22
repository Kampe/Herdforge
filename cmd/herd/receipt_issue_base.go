package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/dispatch"
)

func gitC(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
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

// verifierIssuanceBase picks a base that is an ancestor of the candidate.
// origin/main after later merges is not that base. Prefer an authenticated
// builder base when it is still ancestral; otherwise use merge-base with
// origin/main. An explicit --base must itself be ancestral.
func verifierIssuanceBase(dir, originMain, candidate, authenticatedBase, explicitBase string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", fmt.Errorf("verifier issuance requires a candidate SHA")
	}
	explicitBase = strings.TrimSpace(explicitBase)
	authenticatedBase = strings.TrimSpace(authenticatedBase)
	originMain = strings.TrimSpace(originMain)

	pick := explicitBase
	if pick == "" && authenticatedBase != "" && gitIsAncestor(dir, authenticatedBase, candidate) {
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

func authenticatedBuilderBase(root, targetDir, candidate string) string {
	prior, err := dispatch.ReadTaskContext(targetDir)
	if err != nil {
		return ""
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return ""
	}
	if err := verifier.Verify(prior); err != nil {
		return ""
	}
	if strings.TrimSpace(prior.CandidateSHA) != strings.TrimSpace(candidate) {
		return ""
	}
	role := strings.ToLower(strings.TrimSpace(prior.Role))
	if role != dispatch.RoleWorker && role != dispatch.RoleRecovery {
		return ""
	}
	return strings.TrimSpace(prior.BaseSHA)
}
