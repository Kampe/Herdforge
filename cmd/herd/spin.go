package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/spin"
)

var liveModelMention = regexp.MustCompile(`(?i)\b(grok[\s/_-]*\d+\.\d+(?:[^\n)]*)?|(?:claude|gpt|codex|gemini|deepseek)[\s/_-]*[a-z0-9.-]+)`)

func liveModelFromPane(tail string) string {
	m := liveModelMention.FindStringSubmatch(tail)
	if len(m) == 0 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func recordedModelForAgent(root string, agent herdr.AgentEntry) string {
	receipts, err := launch.ReadReceipts(launch.ReceiptPathFor(root))
	if err != nil {
		return ""
	}
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if !r.Accepted || strings.TrimSpace(r.Model) == "" {
			continue
		}
		if (agent.PaneID != "" && r.PaneID == agent.PaneID) || (agent.Name != "" && r.Name == agent.Name) {
			return strings.TrimSpace(r.Model)
		}
	}
	return ""
}

// continuationsForWorktree reads the durable stop-hook continuation counter
// for the lane checked out at cwd (FAC-628). goalguard's own state path is
// cwd-relative by design (each lane's Stop hook runs with its own worktree
// as cwd, per goalguard.DefaultPath's doc comment), so this joins the SAME
// relative path directly against the pane's observed cwd rather than reusing
// DefaultPath, which would read spin's OWN process cwd instead of the pane's.
//
// Missing or unreadable state is not evidence of an empty loop -- it is the
// normal state for a lane not running under a goal-guarded Stop hook at all
// -- so it returns 0, never an error.
func continuationsForWorktree(cwd string) int64 {
	if strings.TrimSpace(cwd) == "" {
		return 0
	}
	store, err := goalguard.Open(goalguard.PathForCWD(cwd))
	if err != nil {
		return 0
	}
	goal, err := store.Load()
	if err != nil {
		return 0
	}
	return int64(goal.Continuations)
}

// runSpin samples the live fleet and reports which agents are consuming
// resources without durable progress (FAC-90).
//
// Progress is measured from the lifecycle event sequence, the candidate SHA,
// herdr's own state-change counter and the worktree git snapshot — never from
// terminal text, which is carried only as a diagnostic. A sample always exits
// 0: a detector that fails its caller when it finds something makes every
// wrapper treat detection as an error.
func runSpin() {
	fs := flag.NewFlagSet("spin", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Emit the assessment as JSON")
	reset := fs.Bool("reset", false, "Wipe sample state and exit")
	tailLines := fs.Int("tail-lines", 80, "Pane tail lines to fingerprint (diagnostic only)")
	act := fs.Bool("act", false, "Perform the one bounded recovery action policy permits per pane (default: report only)")
	lifecycleDB := fs.String("lifecycle-db", "", "Lifecycle event store (default $HERD_ROOT/.herd/lifecycle.db)")
	fs.Parse(os.Args[2:])

	repoRoot := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	stateFile := filepath.Join(repoRoot, ".herd", "spin-state.json")
	if *reset {
		if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "herd spin: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("herd spin: sample state cleared")
		return
	}

	prior := map[string]spin.Sample{}
	if raw, err := os.ReadFile(stateFile); err == nil {
		// A corrupt state file must not silently reset the act budget: an
		// empty prior means every pane looks new and every rate limit is
		// forgotten. Refuse instead.
		if err := json.Unmarshal(raw, &prior); err != nil {
			fmt.Fprintf(os.Stderr, "herd spin: unreadable sample state %s: %v\n", stateFile, err)
			fmt.Fprintf(os.Stderr, "herd spin: refusing to run with a forgotten act budget; fix or `herd spin --reset`\n")
			os.Exit(1)
		}
	}

	// Only open the lifecycle store if it already exists — sampling the
	// fleet must not create durable state as a side effect. Without it the
	// lifecycle signals are simply absent from the evidence, and the
	// recovery transition is unavailable.
	dbPath := *lifecycleDB
	if dbPath == "" {
		dbPath = filepath.Join(repoRoot, ".herd", "lifecycle.db")
	}
	var machine *lifecycle.Machine
	if _, err := os.Stat(dbPath); err == nil {
		machine, err = lifecycle.NewMachine(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd spin: lifecycle store %s: %v\n", dbPath, err)
			os.Exit(1)
		}
		defer machine.Close()
	}

	agents, err := herdr.AgentList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd spin: agent list: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	harvester := harvest.NewHarvester(repoRoot)
	now := time.Now().UTC()
	current := map[string]spin.Sample{}
	var assessments []spin.Assessment

	for _, a := range agents {
		if a.PaneID == "" {
			continue
		}
		tail, _ := herdr.PaneRead(a.PaneID, *tailLines)
		pid, cwd, alive := paneProcessState(a.PaneID)

		obs := spin.Observation{
			PaneID:        a.PaneID,
			Name:          a.Name,
			AgentStatus:   a.Status,
			PID:           pid,
			ProcAlive:     alive,
			UniqueWork:    spin.TriUnknown,
			Diagnostic:    string(process.ClassifyTarget(a.PaneID, a.Name, a.Status, tail).Class),
			Progress:      spin.Progress{StateChangeSeq: a.StateChangeSeq, Continuations: continuationsForWorktree(cwd)},
			RecordedModel: recordedModelForAgent(repoRoot, a),
			LiveModel:     liveModelFromPane(tail),
		}

		writer := spin.IsWriter(a.Name, cwd)
		if writer && cwd != "" {
			obs.Progress.Head, obs.Progress.Dirty = gitSnapshot(cwd)
			obs.UniqueWork = uniqueWorkState(ctx, harvester, cwd)
		} else if !writer {
			// A non-writer agent owns no branch, so there is no unique work
			// for a recovery to destroy.
			obs.UniqueWork = spin.TriNo
		}

		taskRef := taskRefForWorktree(cwd)
		var taskState *lifecycle.TaskState
		if machine != nil && taskRef != "" {
			if ts, err := machine.EventStore().CurrentState(taskRef); err == nil && ts != nil {
				taskState = ts
				obs.Progress.LifecycleSeq = ts.Seq
				obs.Progress.LifecycleState = string(ts.State)
				obs.Progress.CandidateSHA = ts.CandidateSHA
			}
		}
		obs.RecoveryAvailable = taskState != nil

		// The fingerprint detector still runs, purely so its STALL/SPIN/LONG
		// verdicts ride along as operator diagnostics.
		prev, hadPrev := prior[a.PaneID]
		diag := spin.Sample{
			PaneID: a.PaneID, Name: a.Name, AgentStatus: a.Status,
			Fingerprint: spin.Fingerprint(tail), Writer: writer,
			Head: obs.Progress.Head, Dirty: obs.Progress.Dirty,
		}
		var prevPtr *spin.Sample
		if hadPrev {
			prevPtr = &prev
		}
		if hadPrev && strings.EqualFold(prev.AgentStatus, "working") && strings.EqualFold(a.Status, "working") {
			diag.FirstWorkingUnix = prev.FirstWorkingUnix
		} else if strings.EqualFold(a.Status, "working") {
			diag.FirstWorkingUnix = now.Unix()
		}
		workingFor := time.Duration(0)
		if diag.FirstWorkingUnix > 0 {
			workingFor = now.Sub(time.Unix(diag.FirstWorkingUnix, 0))
		}
		diag, obs.Findings = spin.Classify(prevPtr, diag, spin.DefaultThresholds(), workingFor)

		updated, assessment := spin.Assess(prevPtr, obs, spin.DefaultPolicy(), now, *act)
		// Carry the diagnostic counters forward on the persisted sample.
		updated.Fingerprint, updated.Writer = diag.Fingerprint, diag.Writer
		updated.StallHits, updated.SpinHits = diag.StallHits, diag.SpinHits
		updated.FirstWorkingUnix = diag.FirstWorkingUnix

		if assessment.Acted {
			if err := performSpinAction(assessment, a, machine, taskState); err != nil {
				// The budget was booked before execution on purpose: a side
				// effect we cannot confirm must not be retried on the next
				// sample. Report the failure, keep the slot spent.
				assessment.Withheld = "action failed: " + err.Error()
				assessment.Evidence = append(assessment.Evidence, "delivery error; budget slot consumed")
			}
		}

		current[a.PaneID] = updated
		if assessment.Cause != spin.CauseProgressing {
			assessments = append(assessments, assessment)
		}
	}

	if body, err := json.Marshal(current); err == nil {
		if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err == nil {
			if err := os.WriteFile(stateFile, body, 0o600); err != nil {
				// Losing the write loses the act budget, so say so loudly.
				fmt.Fprintf(os.Stderr, "herd spin: could not persist sample state: %v\n", err)
			}
		}
	}

	if *asJSON {
		body, _ := json.MarshalIndent(assessments, "", "  ")
		fmt.Println(string(body))
		return
	}
	for _, a := range assessments {
		fmt.Printf("%s pane=%s name=%s next=%s acted=%v cycles=%d restarts=%d\n",
			a.Cause, a.PaneID, a.Name, a.NextAction, a.Acted, a.NoProgressCycles, a.RestartCycles)
		for _, e := range a.Evidence {
			fmt.Printf("    evidence: %s\n", e)
		}
		if a.Withheld != "" {
			fmt.Printf("    withheld: %s\n", a.Withheld)
		}
	}
	fmt.Printf("herd spin: sampled=%d reported=%d act=%v\n", len(current), len(assessments), *act)
}

// spinNudgeTimeout bounds how long spin waits for a nudged pane to confirm
// it consumed the prompt.
const spinNudgeTimeout = 30 * time.Second

// performSpinAction executes the one action the policy already permitted.
// It never decides; spin.Assess does.
func performSpinAction(a spin.Assessment, agent herdr.AgentEntry, machine *lifecycle.Machine, ts *lifecycle.TaskState) error {
	switch a.NextAction {
	case spin.ActionNudge:
		// Verified send, not a bare prompt: a submit that lands in a dead
		// pane looks identical to a delivered one, and a nudge nobody
		// consumed must be reported as a failure — spin already spent the
		// budget slot on it.
		_, err := herdr.SendStatus(agent.PaneID, spinNudgeText(a), true, spinNudgeTimeout)
		return err
	case spin.ActionRecover:
		if machine == nil || ts == nil {
			return errors.New("no lifecycle task state; recovery transition unavailable")
		}
		// Same shape as lifecycle.Reconciler: fold the task into Recovering
		// at its own recorded generation, idempotency-keyed on the sequence
		// that proved it stuck, so re-running a sweep replays instead of
		// piling up events.
		_, err := machine.Transition(lifecycle.TransitionRequest{
			TaskRef:         ts.TaskRef,
			Repo:            ts.Repo,
			To:              lifecycle.StateRecovering,
			Actor:           "herd-spin",
			IdempotencyKey:  fmt.Sprintf("spin:%s:%d:%s", ts.TaskRef, ts.Seq, lifecycle.StateRecovering),
			LeaseGeneration: ts.LeaseGeneration,
			Branch:          ts.Branch,
			CandidateSHA:    ts.CandidateSHA,
			EvidenceDigest: fmt.Sprintf("herd spin: %s after %d samples with no durable progress (%d restarts)",
				a.Cause, a.NoProgressCycles, a.RestartCycles),
		})
		return err
	default:
		return fmt.Errorf("spin: action %q is not performable", a.NextAction)
	}
}

func spinNudgeText(a spin.Assessment) string {
	return fmt.Sprintf(
		"HERD SPIN (%s): no durable progress across %d samples. Evidence: %s. "+
			"Report your current blocker in one line, then either continue or report BLOCKED.",
		a.Cause, a.NoProgressCycles, strings.Join(a.Evidence, "; "))
}

// paneProcessState returns the pane's foreground PID, working directory, and
// whether liveness could be established at all. Unknown liveness is a
// fail-closed condition in spin.Assess, so every failure path returns it.
//
// The first foreground process is used for restart detection. When an agent
// is working that process is the agent; when it is not working the pane may
// show its shell instead, but a non-working pane never reaches crash-loop
// classification.
func paneProcessState(paneID string) (pid int, cwd string, alive spin.Tri) {
	procs, err := herdr.PaneProcessInfo(paneID)
	if err != nil || len(procs) == 0 {
		return 0, "", spin.TriUnknown
	}
	alive = spin.TriUnknown
	for _, p := range procs {
		if pid == 0 && p.PID > 0 {
			pid, alive = p.PID, spin.TriYes
		}
		if cwd == "" && p.Cwd != "" {
			cwd = p.Cwd
		}
	}
	return pid, cwd, alive
}

// uniqueWorkState answers "does this worktree hold commits that are not
// upstream". Strict, local, patch-equivalence based: an error is Unknown, not
// No, because a recovery must never be authorized by a failed check.
func uniqueWorkState(ctx context.Context, h *harvest.Harvester, worktree string) spin.Tri {
	work, err := h.UnmergedForStrictLocal(ctx, worktree)
	if err != nil {
		return spin.TriUnknown
	}
	if work != nil && len(work.Unmerged) > 0 {
		return spin.TriYes
	}
	return spin.TriNo
}

// taskRefForWorktree recovers the task ref from the worktree's branch, which
// worktree.TaskBranch minted as herd/<lowercase task ref>. Anything else
// yields "" and the lifecycle signals stay absent rather than guessed.
func taskRefForWorktree(dir string) string {
	if dir == "" {
		return ""
	}
	branch, err := currentBranch(dir)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(branch, "herd/") {
		return ""
	}
	return strings.ToUpper(strings.TrimPrefix(branch, "herd/"))
}

// gitSnapshot returns HEAD and the dirty-file count for a worktree.
func gitSnapshot(dir string) (string, int) {
	head := ""
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	dirty := 0
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(l) != "" {
				dirty++
			}
		}
	}
	return head, dirty
}
