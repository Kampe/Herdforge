package main

import (
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

	"github.com/Kampe/Herdforge/pkg/attention"
	"github.com/Kampe/Herdforge/pkg/candidateindex"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

type attentionCommand func(context.Context, string, ...string) ([]byte, error)

func attentionCommandAt(root string) attentionCommand {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(bounded, name, args...)
		cmd.Dir = root
		out, err := cmd.Output()
		if bounded.Err() != nil {
			return nil, bounded.Err()
		}
		return out, err
	}
}

// readAttentionPRs uses one remote snapshot for head identity and required
// check evidence. Hitting the result limit is UNKNOWN, never proof of absence.
func readAttentionPRs(ctx context.Context, run attentionCommand, branch string) ([]attention.CandidatePR, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("candidate has no recorded PR branch")
	}
	out, err := run(ctx, "gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "100", "--json", "number,headRefOid,state,url,mergeable,statusCheckRollup")
	if err != nil {
		return nil, fmt.Errorf("read PR snapshot: %w", err)
	}
	var views []prView
	if err := json.Unmarshal(out, &views); err != nil {
		return nil, fmt.Errorf("decode PR snapshot: %w", err)
	}
	if views == nil || len(views) >= 100 {
		return nil, fmt.Errorf("PR snapshot is null or may be truncated")
	}
	prs := make([]attention.CandidatePR, 0, len(views))
	for _, v := range views {
		if v.Number <= 0 || v.HeadRefOid == "" || v.State == "" || v.URL == "" {
			return nil, fmt.Errorf("PR snapshot has incomplete identity")
		}
		pr := attention.CandidatePR{Number: v.Number, HeadSHA: v.HeadRefOid, State: v.State, URL: v.URL, Mergeable: v.Mergeable}
		for _, c := range v.StatusCheckRollup {
			name, status, conclusion, url := c.Name, c.Status, c.Conclusion, c.DetailsURL
			if name == "" {
				name = c.Context
				url = c.TargetURL
				// Legacy StatusContext reports state rather than status/conclusion.
				switch strings.ToUpper(c.State) {
				case "PENDING", "EXPECTED":
					status = "PENDING"
				case "SUCCESS", "FAILURE", "ERROR":
					status, conclusion = "COMPLETED", c.State
				}
			}
			pr.Checks = append(pr.Checks, attention.CandidateCheck{Name: name, Status: status, Conclusion: conclusion, URL: url})
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// collectAttentionCandidates joins the canonical queued ready set with live
// task and PR evidence. No worktree census, review launch or provider write.
func collectAttentionCandidates(ctx context.Context, root string, cfg *config.Config, tp provider.TaskProvider, run attentionCommand) ([]attention.CandidateItem, error) {
	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		return nil, err
	}
	queued, err := ledger.Queued()
	if err != nil {
		return nil, err
	}
	if len(queued) == 0 {
		return nil, nil
	}
	rows, err := ledger.AllRows()
	if err != nil {
		return nil, err
	}
	if cfg == nil || tp == nil {
		return nil, fmt.Errorf("task provider context is required")
	}
	records := map[string]reviewledger.LedgerRow{}
	ambiguous := map[string]bool{}
	superseded := map[string]bool{}
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) {
			record := records[row.SHA]
			if record.Task != "" && row.Task != "" && record.Task != row.Task {
				ambiguous[row.SHA] = true
			}
			if record.Branch != "" && row.Branch != "" && record.Branch != row.Branch {
				ambiguous[row.SHA] = true
			}
			if row.Task != "" {
				record.Task = row.Task
			}
			if row.Branch != "" {
				record.Branch = row.Branch
			}
			records[row.SHA] = record
		}
		if row.Event == string(reviewledger.EventSupersession) {
			superseded[row.Task] = true
		}
	}
	policy, err := preflight.LoadMergePolicy(root)
	if err != nil {
		return nil, fmt.Errorf("required-check policy: %w", err)
	}
	envelopes, err := mail.NewMailbox(mail.CallbackMailPath(root)).ReadInboxContext(ctx, mail.CoordinatorInbox)
	if err != nil {
		return nil, fmt.Errorf("callback evidence: %w", err)
	}
	sort.SliceStable(envelopes, func(i, j int) bool { return envelopes[i].Sequence < envelopes[j].Sequence })
	var findings []attention.CandidateItem
	var failures []error
	priorities := map[string]provider.Priority{}
	sort.Slice(queued, func(i, j int) bool { return queued[i].SHA < queued[j].SHA })
	for _, entry := range queued {
		if err := ctx.Err(); err != nil {
			return findings, errors.Join(append(failures, err)...)
		}
		record := records[entry.SHA]
		o := attention.CandidateObservation{SHA: entry.SHA, Branch: record.Branch, Task: record.Task, Superseded: superseded[entry.SHA], RequiredChecks: policy.RequiredChecks}
		if o.Branch == "" {
			o.Branch = entry.Branch
		}
		readiness, readErr := ledger.MergeReadinessFor(entry.SHA)
		if readErr != nil {
			return findings, readErr
		}
		o.ReviewReady = readiness.Ready
		if !o.ReviewReady || o.Superseded {
			continue
		}
		callbackBlocked, unboundCallback := attentionCallbackState(envelopes, o.Task, o.SHA)
		o.CallbackBlocked = callbackBlocked

		fail := func(err error) {
			o.EvidenceError = err.Error()
			failures = append(failures, fmt.Errorf("%s %s: %w", o.Task, o.SHA, err))
		}
		if ambiguous[o.SHA] {
			fail(fmt.Errorf("canonical candidate records disagree on task/branch identity"))
		} else if o.Task == "" {
			fail(fmt.Errorf("canonical record has no task identity"))
		} else {
			taskCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			task, taskErr := tp.GetTask(taskCtx, o.Task)
			cancel()
			switch {
			case taskErr != nil:
				fail(fmt.Errorf("current task: %w", taskErr))
			case task == nil || !strings.EqualFold(task.Ref, o.Task) || task.ProjectID != cfg.TaskProvider.ProjectID:
				fail(fmt.Errorf("current task identity does not match candidate project/ref"))
			default:
				o.TaskStatus = task.Status
				priorities[o.SHA] = task.Priority
			}
		}
		if o.EvidenceError == "" && unboundCallback {
			fail(fmt.Errorf("task has a BLOCKED callback without exact candidate identity"))
		}
		if o.EvidenceError == "" {
			receipt, receiptErr := hsync.LoadReceipt(hsync.ReceiptPath(root, o.Task))
			switch {
			case errors.Is(receiptErr, os.ErrNotExist):
			case receiptErr != nil:
				fail(fmt.Errorf("landing receipt: %w", receiptErr))
			case receipt.TaskRef != o.Task || receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest():
				fail(fmt.Errorf("landing receipt identity or digest is invalid"))
			case receipt.CandidateSHA == o.SHA:
				o.LandingReceipt = receipt.Digest
			}
		}
		if o.EvidenceError == "" && o.LandingReceipt == "" && o.TaskStatus != "done" && o.TaskStatus != "archived" {
			head, headErr := run(ctx, "git", "rev-parse", "--verify", "refs/heads/"+o.Branch+"^{commit}")
			if headErr != nil {
				fail(fmt.Errorf("current branch identity: %w", headErr))
			} else if strings.TrimSpace(string(head)) != o.SHA {
				o.Superseded = true
			}
			if !o.Superseded && o.EvidenceError == "" {
				o.PullRequests, err = readAttentionPRs(ctx, run, o.Branch)
				if err != nil {
					fail(err)
				}
			}
		}
		if item, visible := attention.ClassifyCandidate(o); visible {
			findings = append(findings, item)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		pi, pj := candidateindex.PriorityRank(priorities[findings[i].SHA]), candidateindex.PriorityRank(priorities[findings[j].SHA])
		if pi != pj {
			return pi > pj
		}
		if cmp := provider.CompareRefs(findings[i].Task, findings[j].Task); cmp != 0 {
			return cmp < 0
		}
		return findings[i].SHA < findings[j].SHA
	})
	return findings, errors.Join(failures...)
}

func populateAttentionCandidates(ctx context.Context, result *attention.Result, cfg *config.Config) error {
	root, err := canonicalHerdRoot()
	if err != nil {
		result.CandidateError = err.Error()
		return err
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		result.CandidateError = err.Error()
		return err
	}
	result.Candidates, err = collectAttentionCandidates(ctx, root, cfg, tp, attentionCommandAt(root))
	if err != nil {
		result.CandidateError = err.Error()
		return err
	}
	result.Candidates, err = attention.RecordCandidateBeat(filepath.Join(root, ".herd", "attention-candidates.json"), result.Candidates)
	if err != nil {
		result.CandidateError = err.Error()
	}
	return err
}

// attentionCallbackState keeps an unbound task callback UNKNOWN. An exact
// complete callback clears only earlier evidence from its own generation.
func attentionCallbackState(envelopes []*mail.Envelope, ref, sha string) (blocked, unknown bool) {
	var blockedLease, blockedSequence, unboundLease, unboundSequence int64
	for _, env := range envelopes {
		var cb mail.Callback
		if json.Unmarshal([]byte(env.Body), &cb) != nil || cb.Ref != ref || strings.HasPrefix(cb.DedupeID, mail.VerdictIntentID("")) {
			continue
		}
		if cb.SHA == "" {
			if cb.Kind == mail.CallbackBlocked {
				unknown = true
				unboundLease, unboundSequence = cb.LeaseGeneration, env.Sequence
			}
			continue
		}
		if cb.SHA != sha {
			continue
		}
		if cb.Kind == mail.CallbackBlocked {
			blocked = true
			blockedLease, blockedSequence = cb.LeaseGeneration, env.Sequence
		}
		if cb.Kind == mail.CallbackComplete {
			if cb.LeaseGeneration == blockedLease && env.Sequence > blockedSequence {
				blocked = false
			}
			if cb.LeaseGeneration == unboundLease && env.Sequence > unboundSequence {
				unknown = false
			}
		}
	}
	return blocked, unknown
}
