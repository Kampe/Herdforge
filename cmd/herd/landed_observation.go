package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

// Observe immutable candidate content using the same proof as reconciliation.
// Fetch remains mandatory, and dirty work is refused, but no rebase rewrites
// the candidate or tries to replay intermediate edits already squash-landed.
func observeVerifyLanded(dir string, gate *mergeadmit.Gate, req mergeadmit.Request) (*mergeadmit.Proof, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	status, err := git("status", "--porcelain", gitroot.StatusUntrackedNormal)
	if err != nil {
		return nil, err
	}
	if status != "" {
		return nil, fmt.Errorf("landing proof refuses dirty worktree")
	}
	landed, err := originMainProbe(dir)()
	if err != nil {
		return nil, err
	}
	return gate.ProveLanded(req, landed)
}
