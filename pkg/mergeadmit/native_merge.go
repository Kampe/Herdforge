package mergeadmit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// Publish advances only the merge effect. The compiled gate remains the
// authority; a PR, a clean merge-tree, or an old Decision is not permission.
// Only exact-head fast-forward publication is supported here. A replay that
// changes the reviewed SHA must obtain its own review before reaching this API.
//
// The expected-old lease pins the base at the server's ref transaction. The
// separate ancestry check forbids history replacement, even though Git spells
// the compare-and-swap option --force-with-lease. Branch protection remains
// enforced by the remote; this never requests an administrative bypass.
func (m *NativeMerge) Publish(ctx context.Context) (*harvest.IntegrationPR, error) {
	return m.publish(ctx, nil)
}

// PublishRecorded retains the actual fresh admission before the remote effect.
// A failed persistence callback refuses publication. The callback does not
// replace the compiled gate and cannot authorize an otherwise refused merge.
func (m *NativeMerge) PublishRecorded(ctx context.Context, retain func(Request, Decision) error) (*harvest.IntegrationPR, error) {
	if retain == nil {
		return nil, fmt.Errorf("integration merge: durable admission recorder required")
	}
	return m.publish(ctx, retain)
}

func (m *NativeMerge) publish(ctx context.Context, retain func(Request, Decision) error) (*harvest.IntegrationPR, error) {
	p, gate, req := m.publisher, m.gate, m.request
	if p == nil {
		return nil, fmt.Errorf("integration merge: native PR publisher required")
	}
	if gate == nil {
		return nil, fmt.Errorf("integration merge: compiled admission gate required")
	}
	if req.CandidateSHA != p.Binding.Candidate || req.CandidateSHA != p.Binding.Head || !nativeFullSHA(req.BaseSHA) || req.Mode != ModeMerge {
		return nil, fmt.Errorf("integration merge: exact reviewed head, full base and merge mode required")
	}
	common, err := worktree.GitCommonDir(ctx, p.RepoRoot)
	if err != nil {
		return nil, err
	}
	gateCommon, err := worktree.GitCommonDir(ctx, gate.RepoDir)
	if err != nil || gateCommon != common {
		return nil, fmt.Errorf("integration merge: gate belongs to another repository")
	}
	lockDir := filepath.Join(common, harvest.SharedIntegrationLockName)
	if os.Getenv(lock.EnvHeld) != lockDir {
		dl := lock.NewDirLock(lockDir)
		if err := dl.Acquire(ctx, 0, "publish admitted integration merge"); err != nil {
			return nil, err
		}
		defer dl.Release()
	}
	pr, err := m.observe(ctx)
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.State != "OPEN" {
		return nil, fmt.Errorf("integration merge: exact open PR required; observe a completed effect instead of executing it again")
	}
	decision, err := gate.Admit(req)
	if err != nil {
		return nil, fmt.Errorf("integration merge: compiled admission refused: %w", err)
	}
	if decision == nil || !decision.Admitted {
		return nil, fmt.Errorf("integration merge: compiled gate returned no admitted decision")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := gitroot.RequireAncestorContext(checkCtx, p.RepoRoot, req.BaseSHA, p.Binding.Head); err != nil {
		return nil, fmt.Errorf("integration merge: candidate is not a fast-forward of its admitted base: %w", err)
	}
	baseRef := "refs/heads/" + p.Binding.Base
	remote, err := m.run(ctx, 15*time.Second, "git", []string{"ls-remote", "--heads", "--refs", "origin", baseRef}, "")
	if err != nil {
		return nil, fmt.Errorf("integration merge: remote base UNKNOWN: %w", err)
	}
	fields := strings.Fields(string(remote))
	if len(fields) != 2 || fields[0] != req.BaseSHA || fields[1] != baseRef {
		return nil, fmt.Errorf("integration merge: remote base changed or is ambiguous; restart admission")
	}
	if retain != nil {
		if err := retain(req, *decision); err != nil {
			return nil, fmt.Errorf("integration merge: retain admission before publication: %w", err)
		}
	}
	if _, err := m.run(ctx, 2*time.Minute, "git", []string{"push", gitroot.RefLeaseFlagPrefix + baseRef + ":" + req.BaseSHA, "origin", p.Binding.Head + ":" + baseRef}, ""); err != nil {
		return nil, fmt.Errorf("integration merge: publication outcome requires readback: %w", err)
	}
	pr, err = m.observe(ctx)
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.State != "MERGED" {
		return nil, fmt.Errorf("integration merge: published head has no exact merged PR readback yet")
	}
	return pr, nil
}

// NativeMerge composes native PR readback with the compiled admission gate.
// Publication can never be enabled with a boolean assertion in place of Gate.
type NativeMerge struct {
	publisher *harvest.IntegrationPRPublisher
	gate      *Gate
	request   Request
	observePR func(context.Context) (*harvest.IntegrationPR, error)
	command   func(context.Context, string, []string) ([]byte, error)
}

func NewNativeMerge(p *harvest.IntegrationPRPublisher, g *Gate, req Request) *NativeMerge {
	return &NativeMerge{publisher: p, gate: g, request: req}
}
func (m *NativeMerge) observe(ctx context.Context) (*harvest.IntegrationPR, error) {
	if m.observePR != nil {
		return m.observePR(ctx)
	}
	return m.publisher.Observe(ctx)
}
func (m *NativeMerge) run(ctx context.Context, limit time.Duration, name string, args []string, input string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.command != nil {
		return m.command(ctx, name, args)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = m.publisher.RepoRoot
	return cmd.Output()
}
func nativeFullSHA(sha string) bool {
	return len(sha) == 40 && strings.Trim(sha, "0123456789abcdef") == ""
}
