package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/toolchild"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// nativeIntegrationSteps is the production adapter, not a second transaction
// engine. Integration owns order/intent/locks; these callbacks observe or apply
// exactly one native effect. No command runs in the shared source checkout.
type nativeIntegrationSteps struct {
	root, project string
	tasks         provider.TaskProvider
	ledger        *reviewledger.Ledger
}

type nativeIntegrationPlan struct {
	Repository string             `json:"repository"`
	Branch     string             `json:"branch"`
	Worktree   string             `json:"worktree"`
	Request    mergeadmit.Request `json:"request"`
}

type nativeIntegrationMerge struct {
	PR        harvest.IntegrationPR `json:"pr"`
	Admission admissionRecord       `json:"admission"`
}

func (n *nativeIntegrationSteps) manager() *worktree.WorktreeManager {
	return worktree.NewWorktreePool(n.root, filepath.Join(n.root, ".herd", "integration-worktrees"))
}
func (n *nativeIntegrationSteps) plan(b harvest.IntegrationBatch, tx integration.Transaction, i integration.Intent) (nativeIntegrationPlan, error) {
	var p nativeIntegrationPlan
	for _, r := range tx.Done {
		if r.Step == integration.StepHarvest {
			if err := json.Unmarshal([]byte(r.Evidence), &p); err != nil {
				return p, err
			}
			return p, n.validatePlan(b, p)
		}
	}
	if i.Step == integration.StepHarvest && i.ID != "" {
		present, err := readNativeIntegrationState(n.root, i, "plan", &p)
		if err != nil || present {
			if err == nil {
				err = n.validatePlan(b, p)
			}
			return p, err
		}
	}
	// The existing task-bound admission record supplies reviewed context. It
	// is not permission to publish this new PR: merge re-runs the concrete gate.
	r, err := readAdmissionRecord(n.root, b.Task)
	if err != nil {
		return p, fmt.Errorf("integration: retain exact merge-admit context for %s before entry: %w", b.Task, err)
	}
	repository, err := toolchild.RepositoryIdentity(n.root)
	if err != nil {
		return p, err
	}
	p = nativeIntegrationPlan{Repository: repository, Branch: harvest.IntegrationNamespace + strings.ToLower(b.Task) + "-" + b.Candidate[:12], Worktree: ".herd/integration-worktrees/" + b.Candidate, Request: r.Request}
	return p, n.validatePlan(b, p)
}
func (n *nativeIntegrationSteps) validatePlan(b harvest.IntegrationBatch, p nativeIntegrationPlan) error {
	r := p.Request
	if r.Ref != b.Task || r.CandidateSHA != b.Candidate || len(r.BaseSHA) != 40 || r.TaskID == "" || r.ProviderRevision == "" || r.AcceptanceDigest == "" || r.Lease == "" || r.LeaseGeneration <= 0 || r.Mode != mergeadmit.ModeMerge || r.ReducedProvenance != nil || r.Reconstruction != nil {
		return fmt.Errorf("integration: exact reviewed task/base/lease context and native merge mode required")
	}
	if p.Worktree != ".herd/integration-worktrees/"+b.Candidate || p.Branch != harvest.IntegrationNamespace+strings.ToLower(b.Task)+"-"+b.Candidate[:12] {
		return fmt.Errorf("integration: retained worktree or branch identity changed")
	}
	current, err := toolchild.RepositoryIdentity(n.root)
	if err != nil || current != p.Repository {
		return fmt.Errorf("integration: retained repository identity changed")
	}
	return nil
}
func (n *nativeIntegrationSteps) publisher(p nativeIntegrationPlan) *harvest.IntegrationPRPublisher {
	return &harvest.IntegrationPRPublisher{RepoRoot: n.root, Binding: harvest.IntegrationPRBinding{Repository: p.Repository, Candidate: p.Request.CandidateSHA, Head: p.Request.CandidateSHA, Branch: p.Branch, Base: "main"}}
}
func (n *nativeIntegrationSteps) Check(ctx context.Context, b harvest.IntegrationBatch, tx integration.Transaction, i integration.Intent) error {
	if n.tasks == nil || n.ledger == nil || n.project == "" {
		return fmt.Errorf("integration: native task, ledger and project authorities required")
	}
	p, err := n.plan(b, tx, i)
	if err != nil {
		return err
	}
	if i.Step == integration.StepPass || i.Step == integration.StepHarvest || i.Step == integration.StepIntegrationPR {
		main, err := n.remoteMain(ctx)
		if err != nil {
			return err
		}
		if main != p.Request.BaseSHA {
			return fmt.Errorf("integration: reviewed base advanced; retain source and obtain exact rebased review")
		}
		if err := gitroot.RequireAncestorContext(ctx, n.root, main, b.Candidate); err != nil {
			return err
		}
	}
	if i.Step == integration.StepCleanup && !tx.Completed(integration.StepPatchProof) {
		return fmt.Errorf("integration: cleanup requires recorded patch-identity proof")
	}
	return nil
}
func (n *nativeIntegrationSteps) Observe(ctx context.Context, b harvest.IntegrationBatch, tx integration.Transaction, i integration.Intent) (integration.Observation, error) {
	o := integration.Observation{Intent: i, State: integration.EffectAbsent}
	p, err := n.plan(b, tx, i)
	if err != nil {
		return o, err
	}
	var evidence any
	switch i.Step {
	case integration.StepHarvest:
		got, err := n.manager().ObserveExactWorktree(ctx, p.Branch, filepath.Join(n.root, p.Worktree), b.Candidate)
		if err != nil || got == nil {
			return o, err
		}
		var retained nativeIntegrationPlan
		present, err := readNativeIntegrationState(n.root, i, "plan", &retained)
		if err != nil || !present {
			return o, fmt.Errorf("integration: existing harvest has no retained operation plan: %v", err)
		}
		evidence = p
	case integration.StepIntegrationPR, integration.StepMerge:
		pr, err := n.publisher(p).Observe(ctx)
		if err != nil || pr == nil {
			return o, err
		}
		if i.Step == integration.StepIntegrationPR {
			evidence = pr
		} else {
			if pr.State != "MERGED" {
				return o, nil
			}
			var admission admissionRecord
			present, err := readNativeIntegrationState(n.root, i, "admission", &admission)
			if err != nil || !present || !admission.Decision.Admitted || admission.Request.CandidateSHA != b.Candidate || admission.Request.BaseSHA != p.Request.BaseSHA {
				return o, fmt.Errorf("integration: merged PR lacks this operation's retained fresh admission: %v", err)
			}
			if pr.MergeCommit.OID != b.Candidate {
				return o, fmt.Errorf("integration: native fast-forward merge has a different landed identity")
			}
			evidence = nativeIntegrationMerge{PR: *pr, Admission: admission}
		}
	case integration.StepRuntimeBind:
		m, err := nativeIntegrationMerged(tx)
		if err != nil {
			return o, err
		}
		// A published merge may survive a crash before Git updates the local
		// tracking ref. Prove the remote result before treating that known stale
		// cache as an unfinished part of this step. Failed reads stay UNKNOWN.
		needsFetch, err := n.runtimeNeedsFetch(ctx, m.PR.MergeCommit.OID)
		if err != nil || needsFetch {
			return o, err
		}
		binding, err := (harvest.HerdRuntimeInstaller{Root: n.root, Source: filepath.Join(n.root, p.Worktree), Revision: m.PR.MergeCommit.OID}).ObserveInstallation(ctx)
		if err != nil || binding == nil {
			return o, err
		}
		evidence = binding
	case integration.StepPatchProof:
		receipt, err := n.proofReceipt(ctx, p, tx)
		if err != nil || receipt == nil {
			return o, err
		}
		evidence = receipt
	case integration.StepCleanup:
		if err := n.checkCleanup(ctx, b, p, tx); err != nil {
			return o, err
		}
		done, err := n.cleanupDone(ctx, b, p)
		if err != nil || !done {
			return o, err
		}
		evidence = struct{ Candidate, Task string }{b.Candidate, b.Task}
	default:
		return o, fmt.Errorf("integration: unsupported native observation %s", i.Step)
	}
	raw, err := json.Marshal(evidence)
	o.State, o.Evidence = integration.EffectApplied, string(raw)
	return o, err
}
func (n *nativeIntegrationSteps) Execute(ctx context.Context, b harvest.IntegrationBatch, tx integration.Transaction, i integration.Intent) error {
	p, err := n.plan(b, tx, i)
	if err != nil {
		return err
	}
	switch i.Step {
	case integration.StepHarvest:
		if err := retainNativeIntegrationState(n.root, i, "plan", p); err != nil {
			return err
		}
		_, err = n.manager().CreateExactWorktree(ctx, p.Branch, filepath.Join(n.root, p.Worktree), b.Candidate)
	case integration.StepIntegrationPR:
		_, err = n.publisher(p).Publish(ctx, "Integrate "+b.Task, "Exact reviewed candidate: "+b.Candidate+"\nReviewed base: "+p.Request.BaseSHA+"\nNative integration transaction; merge admission remains required.\n")
	case integration.StepMerge:
		pr, probeErr := n.publisher(p).Observe(ctx)
		if probeErr != nil || pr == nil {
			return fmt.Errorf("integration: open PR unavailable: %v", probeErr)
		}
		gate, gateErr := n.gate(ctx, p, pr.Number)
		if gateErr != nil {
			return gateErr
		}
		_, err = mergeadmit.NewNativeMerge(n.publisher(p), gate, p.Request).PublishRecorded(ctx, func(req mergeadmit.Request, d mergeadmit.Decision) error {
			return retainNativeIntegrationState(n.root, i, "admission", admissionRecord{Request: req, Decision: d})
		})
	case integration.StepRuntimeBind:
		m, mergeErr := nativeIntegrationMerged(tx)
		if mergeErr != nil {
			return mergeErr
		}
		source := filepath.Join(n.root, p.Worktree)
		if _, err = nativeIntegrationCommand(ctx, source, 2*time.Minute, "git", "fetch", "--no-tags", "origin", "main"); err != nil {
			return err
		}
		// Only Herdforge's own module has this runtime contract. A consumer
		// application must supply its real deployment adapter, never a ref bump.
		module, moduleErr := nativeIntegrationCommand(ctx, source, 15*time.Second, "go", "list", "-m")
		if moduleErr != nil || strings.TrimSpace(string(module)) != "github.com/Kampe/Herdforge" {
			return fmt.Errorf("integration: repository has no native Herdforge runtime build contract")
		}
		if _, err = nativeIntegrationCommand(ctx, source, 10*time.Minute, "sh", "./scripts/build-herd.sh", "."); err != nil {
			return err
		}
		_, err = (harvest.HerdRuntimeInstaller{Root: n.root, Source: source, Revision: m.PR.MergeCommit.OID}).Install(ctx)
	case integration.StepPatchProof:
		m, mergeErr := nativeIntegrationMerged(tx)
		if mergeErr != nil {
			return mergeErr
		}
		gate, gateErr := n.gate(ctx, p, 0)
		if gateErr != nil {
			return gateErr
		}
		_, err = gate.Complete(&m.Admission.Decision, m.Admission.Request)
	case integration.StepCleanup:
		if err = n.checkCleanup(ctx, b, p, tx); err != nil {
			return err
		}
		if err = n.boardComplete(ctx, p); err != nil {
			return err
		}
		if err = n.removeExactSource(ctx, b.Worktree, b.Branch, b.Candidate); err != nil {
			return err
		}
		err = n.removeIntegrationSource(ctx, b, p, tx)
	default:
		err = fmt.Errorf("integration: unsupported native execution %s", i.Step)
	}
	return err
}

func nativeIntegrationMerged(tx integration.Transaction) (nativeIntegrationMerge, error) {
	var m nativeIntegrationMerge
	for _, r := range tx.Done {
		if r.Step == integration.StepMerge {
			if err := json.Unmarshal([]byte(r.Evidence), &m); err != nil {
				return m, err
			}
			if m.PR.State != "MERGED" || m.PR.Head != tx.Candidate || m.PR.MergeCommit.OID != tx.Candidate || !m.Admission.Decision.Admitted || m.Admission.Request.CandidateSHA != tx.Candidate {
				return m, fmt.Errorf("integration: recorded merge identity is inconsistent")
			}
			return m, nil
		}
	}
	return m, fmt.Errorf("integration: recorded native merge required")
}

func nativeIntegrationCommand(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("integration: %s failed: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func (n *nativeIntegrationSteps) remoteMain(ctx context.Context) (string, error) {
	out, err := nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "ls-remote", "--heads", "--refs", "origin", gitroot.MainBranchRef)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || len(f[0]) != 40 || f[1] != gitroot.MainBranchRef {
		return "", fmt.Errorf("integration: remote main identity is unknown")
	}
	return f[0], nil
}

func nativeIntegrationStatePath(root string, i integration.Intent, kind string) string {
	return filepath.Join(integration.StoreDir(root), "native", i.Candidate+"-"+i.ID+"-"+kind+".json")
}
func readNativeIntegrationState(root string, i integration.Intent, kind string, into any) (bool, error) {
	raw, err := os.ReadFile(nativeIntegrationStatePath(root, i, kind))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, into)
}
func retainNativeIntegrationState(root string, i integration.Intent, kind string, value any) error {
	if len(i.Candidate) != 40 || len(i.ID) != 32 {
		return fmt.Errorf("integration: durable native intent identity required")
	}
	path := nativeIntegrationStatePath(root, i, kind)
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if prior, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(raw, prior) {
			return fmt.Errorf("integration: retained native evidence cannot be replaced")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".native-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(raw); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (n *nativeIntegrationSteps) gate(ctx context.Context, p nativeIntegrationPlan, pr int) (*mergeadmit.Gate, error) {
	policy, err := preflight.LoadMergePolicy(n.root)
	if err != nil {
		return nil, err
	}
	live := mergeadmit.LiveState{OriginMain: func() (string, error) { return n.remoteMain(ctx) }}
	if pr > 0 {
		probes := &prProbes{number: pr, root: n.root, repository: p.Repository, ctx: ctx}
		live.CandidateHead, live.Mergeable, live.Checks = probes.head, probes.mergeable, probes.checks
	}
	live.TaskRevision = func() (string, error) {
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		task, err := n.tasks.GetTask(readCtx, p.Request.TaskID)
		if err != nil || task == nil {
			return "", fmt.Errorf("integration: exact task revision UNKNOWN: %v", err)
		}
		if task.ID != p.Request.TaskID || task.Ref != p.Request.Ref {
			return "", fmt.Errorf("integration: provider returned another task identity")
		}
		return mergeadmit.TaskContentRevision(task.Ref, task.Title, task.Description), nil
	}
	gate := &mergeadmit.Gate{RepoDir: n.root, Policy: policy, Ledger: n.ledger, Live: live}
	if policy.RemoteCI.Required {
		checks := append([]string(nil), policy.RemoteCI.RequiredChecks...)
		sort.Strings(checks)
		gate.RemoteCIRepository = p.Repository
		gate.RemoteCIPolicyRevision = remoteci.Revision(preflight.PolicyRevision(policy), strings.Join(checks, "\x00"))
	}
	return gate, nil
}

// runtimeNeedsFetch distinguishes a stale local cache from unknown remote
// publication. Execute owns the fetch; this observation never moves a ref.
func (n *nativeIntegrationSteps) runtimeNeedsFetch(ctx context.Context, landed string) (bool, error) {
	main, err := n.remoteMain(ctx)
	if err != nil {
		return false, err
	}
	if err := gitroot.RequireAncestorContext(ctx, n.root, landed, main); err != nil {
		return false, fmt.Errorf("integration: runtime revision is not on current remote main: %w", err)
	}
	// --verify --quiet distinguishes an absent ref (1) from a failed Git read.
	raw, err := nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/main")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(raw)) == 0 {
			return true, nil
		}
		return false, err
	}
	raw, err = nativeIntegrationCommand(ctx, n.root, 15*time.Second, "git", "merge-base", "--is-ancestor", landed, "origin/main")
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(bytes.TrimSpace(raw)) == 0 {
		return true, nil
	}
	return false, err
}
