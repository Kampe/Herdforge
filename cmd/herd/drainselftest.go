package main

// FAC-184: `herd drain --selftest` exercises the compiled action adapters
// hermetically — fake board provider, fake process API, fake integration, a
// real temp review ledger — and asserts both that a bounded review launch and
// a dry-run harvest complete, and that each fail-closed gate still refuses.
// It needs no bin/ script, no sibling repository, and no live fleet.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
)

const drainSelftestSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeDrainProvider is a board provider with no network and no board.
type fakeDrainProvider struct {
	byStatus map[string][]*provider.Task
	err      error
}

func (f *fakeDrainProvider) ListTasks(_ context.Context, _ string, status string) ([]*provider.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byStatus[status], nil
}
func (f *fakeDrainProvider) GetTask(context.Context, string) (*provider.Task, error) {
	return nil, fmt.Errorf("fake provider: GetTask is not part of the drain path")
}
func (f *fakeDrainProvider) ClaimTask(context.Context, string, string) error {
	return fmt.Errorf("fake provider: the drain never claims")
}
func (f *fakeDrainProvider) UpdateStatus(context.Context, string, string) error {
	return fmt.Errorf("fake provider: the drain never mutates the board directly")
}
func (f *fakeDrainProvider) AddComment(context.Context, string, string) error {
	return fmt.Errorf("fake provider: the drain never comments")
}

// fakeDrainLauncher is a process API that starts nothing.
type fakeDrainLauncher struct {
	agent   string
	receipt func(*router.LaunchDecision) string
	err     error
	packets []string
}

func (f *fakeDrainLauncher) LaunchReviewer(_ context.Context, req launch.Request, packet string) (drainLaunchProof, error) {
	if f.err != nil {
		return drainLaunchProof{}, f.err
	}
	f.packets = append(f.packets, packet)
	receipt := ""
	if f.receipt != nil {
		receipt = f.receipt(req.Decision)
	}
	return drainLaunchProof{Agent: f.agent, Receipt: receipt}, nil
}

// newDrainSelftestAdapters wires a fully authorized fake beat. Individual
// cases then remove exactly one authority to prove it was load-bearing.
func newDrainSelftestAdapters(dir string) (*drainAdapters, *fakeDrainProvider, *fakeDrainLauncher, error) {
	tasks := &fakeDrainProvider{byStatus: map[string][]*provider.Task{
		"in-progress": {{ID: "id-1", Ref: "FAC-1", Title: "candidate"}},
		"in-review":   nil,
	}}
	launcher := &fakeDrainLauncher{agent: "forge-review", receipt: launch.DecisionDigest}
	ledger, err := reviewledger.NewReviewLedger(dir, filepath.Join(dir, "review-ledger.jsonl"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("selftest review ledger: %w", err)
	}
	a := &drainAdapters{
		root:       dir,
		project:    "selftest",
		repository: "selftest-repo",
		cap:        2,
		lane:       &config.LaneDef{Name: "review", Role: launch.ReviewerRole, Worktree: ".herd/worktrees/review"},
		tasks:      tasks,
		ledger:     ledger,
		launcher:   launcher,
		head: func(context.Context, string) (string, error) {
			return drainSelftestSHA, nil
		},
		patchID:     func(context.Context, string) (string, error) { return "patch-" + drainSelftestSHA, nil },
		authorModel: func(string) (string, error) { return "fake-builder-model", nil },
		route: func(_ *config.LaneDef, task *provider.Task) (*router.LaunchDecision, error) {
			d := &router.LaunchDecision{Role: router.RoleReviewer, Shape: "qa", Provider: "fake", Model: "fake-reviewer-model", Effort: "medium", Family: "google", LeaseGeneration: 7}
			for _, label := range task.Labels {
				if strings.HasPrefix(label, "candidate-sha:") {
					d.CandidateSHA = strings.TrimPrefix(label, "candidate-sha:")
				}
			}
			return d, nil
		},
		run: func(_ context.Context, sha string, adm harvest.AdmissionContext, dry bool) (*harvest.IntegrationResult, error) {
			if adm.Task == "" || adm.Lease == "" || adm.PatchURL == "" {
				return nil, fmt.Errorf("integration was handed empty admission context")
			}
			res := &harvest.IntegrationResult{ReviewGatedSHAs: []harvest.ReviewGateOutcome{{SHA: sha, Eligible: true, Reason: "fake admission"}}}
			if !dry {
				res.MergedSHAs = []harvest.MergeOutcome{{SHA: sha, Pushed: true, MergeSHA: "merge-" + sha}}
			}
			return res, nil
		},
	}
	return a, tasks, launcher, nil
}

// drainSelftestEvidence is the one candidate the fake beat acts on.
func drainSelftestEvidence() drainActionEvidence {
	return drainActionEvidence{SHA: drainSelftestSHA, Branch: "task/FAC-1-candidate", Lane: "review", BuilderFamily: "anthropic", Tier: "R1", TierRecorded: true, HarvestReady: true}
}

type drainSelftestCase struct {
	name    string
	mutate  func(*drainAdapters, *fakeDrainProvider, *fakeDrainLauncher, *drainActionEvidence)
	action  func(*drainAdapters, drainActionEvidence) error
	wantErr string
}

func drainReviewAction(a *drainAdapters, e drainActionEvidence) error {
	return a.launchReview(context.Background(), e)
}
func drainDryRunAction(a *drainAdapters, e drainActionEvidence) error {
	return a.integrate(context.Background(), e, true)
}

// drainSelftestCases is the shared table: the runtime selftest and the unit
// tests assert the same gates, so neither can drift into a weaker check.
func drainSelftestCases() []drainSelftestCase {
	return []drainSelftestCase{
		{name: "bounded review launch", action: drainReviewAction},
		{name: "missing board authority", action: drainReviewAction, wantErr: "missing board provider authority",
			mutate: func(a *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, _ *drainActionEvidence) {
				a.tasks = nil
			}},
		{name: "missing process API authority", action: drainReviewAction, wantErr: "missing process API authority",
			mutate: func(a *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, _ *drainActionEvidence) {
				a.launcher = nil
			}},
		{name: "stale candidate SHA", action: drainReviewAction, wantErr: "stale candidate",
			mutate: func(a *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, _ *drainActionEvidence) {
				a.head = func(context.Context, string) (string, error) { return strings.Repeat("f", 40), nil }
			}},
		{name: "inexact candidate SHA", action: drainReviewAction, wantErr: "exact candidate SHA is required",
			mutate: func(_ *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, e *drainActionEvidence) {
				e.SHA = drainSelftestSHA[:12]
			}},
		{name: "unknown builder family", action: drainReviewAction, wantErr: "unknown recorded builder family",
			mutate: func(_ *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, e *drainActionEvidence) {
				e.BuilderFamily = "acme"
			}},
		{name: "same-family reviewer", action: drainReviewAction, wantErr: "must differ from builder family",
			mutate: func(a *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, e *drainActionEvidence) {
				e.BuilderFamily = "google"
			}},
		{name: "branch is not a task ref", action: drainReviewAction, wantErr: "no live board task",
			mutate: func(_ *drainAdapters, _ *fakeDrainProvider, _ *fakeDrainLauncher, e *drainActionEvidence) {
				e.Branch = "wt/lane-9"
			}},
		{name: "live cap drift", action: drainReviewAction, wantErr: "live review cap drift",
			mutate: func(_ *drainAdapters, p *fakeDrainProvider, _ *fakeDrainLauncher, _ *drainActionEvidence) {
				p.byStatus["in-review"] = []*provider.Task{{ID: "a", Ref: "FAC-8"}, {ID: "b", Ref: "FAC-9"}}
			}},
		{name: "absent exact receipt", action: drainReviewAction, wantErr: "no exact durable launch receipt",
			mutate: func(_ *drainAdapters, _ *fakeDrainProvider, l *fakeDrainLauncher, _ *drainActionEvidence) {
				l.receipt = nil
			}},
		{name: "harvest without launch record", action: drainDryRunAction, wantErr: "no durable review launch record"},
	}
}

// drainSelftest runs the fake beat. The first case launches a review, which
// writes the durable launch record the following dry-run harvest admits
// against — the two halves of the bounded beat, in order.
func drainSelftest(out io.Writer) int {
	dir, err := os.MkdirTemp("", "herd-drain-selftest")
	if err != nil {
		fmt.Fprintf(out, "herd-drain selftest FAIL: temp state: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	failed := false
	for _, tc := range drainSelftestCases() {
		caseDir, err := os.MkdirTemp(dir, "case")
		if err != nil {
			fmt.Fprintf(out, "herd-drain selftest FAIL: %s: temp state: %v\n", tc.name, err)
			failed = true
			continue
		}
		a, tasks, launcher, err := newDrainSelftestAdapters(caseDir)
		if err != nil {
			fmt.Fprintf(out, "herd-drain selftest FAIL: %s: %v\n", tc.name, err)
			failed = true
			continue
		}
		e := drainSelftestEvidence()
		if tc.mutate != nil {
			tc.mutate(a, tasks, launcher, &e)
		}
		err = tc.action(a, e)
		if got := drainSelftestVerdict(tc, err); got != "" {
			fmt.Fprintf(out, "herd-drain selftest FAIL: %s: %s\n", tc.name, got)
			failed = true
			continue
		}
		fmt.Fprintf(out, "herd-drain selftest ok: %s\n", tc.name)
	}

	// The bounded beat end to end: a fake review launch, then a dry-run
	// harvest that admits against the record that launch just wrote.
	beatDir, err := os.MkdirTemp(dir, "beat")
	if err == nil {
		var a *drainAdapters
		a, _, _, err = newDrainSelftestAdapters(beatDir)
		if err == nil {
			e := drainSelftestEvidence()
			if err = a.launchReview(context.Background(), e); err == nil {
				err = a.integrate(context.Background(), e, true)
			}
		}
	}
	if err != nil {
		fmt.Fprintf(out, "herd-drain selftest FAIL: bounded review + dry-run harvest beat: %v\n", err)
		failed = true
	} else {
		fmt.Fprintln(out, "herd-drain selftest ok: bounded review + dry-run harvest beat")
	}

	if failed {
		fmt.Fprintln(out, "herd-drain selftest: FAIL")
		return 1
	}
	fmt.Fprintln(out, "herd-drain selftest: PASS (compiled adapters, no bin/ scripts, no sibling repository)")
	return 0
}

func drainSelftestVerdict(tc drainSelftestCase, err error) string {
	if tc.wantErr == "" {
		if err != nil {
			return fmt.Sprintf("expected success, got %v", err)
		}
		return ""
	}
	if err == nil {
		return fmt.Sprintf("expected refusal %q, got success", tc.wantErr)
	}
	if !strings.Contains(err.Error(), tc.wantErr) {
		return fmt.Sprintf("expected refusal %q, got %v", tc.wantErr, err)
	}
	return ""
}
