package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type recoveredWorktree struct {
	root string
	info *worktree.WorktreeInfo
}

func (w recoveredWorktree) CreateTaskWorktreeFrom(context.Context, string, string) (*worktree.WorktreeInfo, error) {
	if w.info == nil {
		return nil, fmt.Errorf("recovered worktree info is required")
	}
	return w.info, nil
}

func (w recoveredWorktree) RepoRoot() string { return w.root }

type releaseTrackingOwnership struct {
	mu         sync.Mutex
	generation int64
	owned      bool
	releases   int
}

func (o *releaseTrackingOwnership) ClaimExclusive(_ context.Context, taskID deps.TaskID, taskRef deps.Ref, role, graphRev, providerRev, _ string) (*deps.OwnershipToken, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.owned = true
	return &deps.OwnershipToken{TaskID: taskID, TaskRef: taskRef, OwnerID: "fac-666-owner", Generation: o.generation, GraphRev: graphRev, ProviderRev: providerRev, Role: role}, nil
}

func (o *releaseTrackingOwnership) StillOwns(context.Context, *deps.OwnershipToken) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.owned, nil
}

func (o *releaseTrackingOwnership) ReleaseIfOwner(context.Context, *deps.OwnershipToken, string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.owned {
		o.owned = false
		o.releases++
	}
	return nil
}

func (o *releaseTrackingOwnership) Close() error { return nil }

func (o *releaseTrackingOwnership) releaseCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.releases
}

func artifactContext(t *testing.T, signer *Signer, generation int64, leaseID string) TaskContext {
	t.Helper()
	tc := validTaskContext()
	tc.TaskRef = "FAC-666"
	tc.TaskID = "task-666"
	tc.ProjectID = "project-666"
	tc.Branch = "herd/fac-666"
	tc.BaseSHA = "base-666"
	tc.LeaseID = leaseID
	tc.LeaseTaskRef = tc.TaskRef
	tc.LeaseGeneration = generation
	tc.SessionID = NewSessionID(tc.Role, tc.TaskRef, tc.BaseSHA, leaseID)
	return mustIssue(t, signer, tc)
}

func artifactPacket(tc TaskContext) string {
	task := &provider.Task{ID: tc.TaskID, Ref: tc.TaskRef, Title: "Task " + tc.TaskRef}
	lane := &config.LaneDef{Name: "worker", Role: RoleWorker, Prompt: ".herd/prompts/worker.md"}
	return buildTaskPacket(task, tc.Branch, lane.Prompt, tc.ProviderType, tc.ProjectID, lane,
		config.Verification{TestCommand: "go test ./..."}, ReplyTarget{Name: "coordinator", LeaseGeneration: tc.LeaseGeneration})
}

func installTaskArtifactIgnores(t *testing.T, repo string) {
	t.Helper()
	ignore := "TASK-CONTEXT.json\nTASK-PACKET.md\n.herd/receipts/\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(ignore), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "test: mirror task artifact ignores")
	runGit(t, repo, "push", "origin", "main")
}

// FAC-666 production-shaped regression: generation 2 left a complete packet
// and signed context in a safely reset worktree. The recovered dispatch owns a
// new external claim generation (3), while the internal dispatch mutex still
// reports generation 2. Every generated packet identity must come from the new
// signed context, never from stale internal-generation state.
func TestDispatchRecoveredWorktreeReplacesGenerationBoundPacketAndContext(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	installTaskArtifactIgnores(t, repo)
	task := &provider.Task{
		ID:          "task-666",
		Ref:         "FAC-666",
		Title:       "Recovered packet replacement",
		Status:      provider.StatusToDo,
		Description: emptyDepsFence("FAC-666", "task-666"),
	}
	tp := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{task}}}
	cfg := testCfg()
	cfg.TaskProvider = config.TaskProvider{Type: "kaneo", ProjectID: "project-666"}

	wtInfo, err := wm.CreateTaskWorktreeFrom(context.Background(), task.Ref, "main")
	if err != nil {
		t.Fatalf("create recovery fixture worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wtInfo.Path).Run()
		_ = os.RemoveAll(wtInfo.Path)
	})

	d := NewDispatcher(cfg, tp, wm)
	d.Worktree = recoveredWorktree{root: repo, info: wtInfo}
	d.Compensator = &recordingCompensator{}
	d.Herdr = &fakeHerdr{available: false}
	d.Ownership = &fixedGenerationOwnership{generation: 2}

	lane := &cfg.Lanes[0]
	oldOpts := DispatchOptions{TicketRef: task.Ref, NoLaunch: true, LeaseID: "claim:66", LeaseGeneration: 2}
	oldContext, err := d.signReceipt(d.taskContext(task, wtInfo, wtInfo.Branch, lane, oldOpts))
	if err != nil {
		t.Fatalf("sign generation-2 context: %v", err)
	}
	if err := WriteTaskContext(wtInfo.Path, oldContext); err != nil {
		t.Fatalf("write generation-2 context: %v", err)
	}
	oldPacket := buildTaskPacket(task, wtInfo.Branch, lane.Prompt, cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID, lane, cfg.Verification, ReplyTarget{
		Name:            "coordinator",
		LeaseGeneration: 2,
	})
	if err := os.WriteFile(filepath.Join(wtInfo.Path, "TASK-PACKET.md"), []byte(oldPacket), 0o644); err != nil {
		t.Fatalf("write generation-2 packet: %v", err)
	}

	result, err := d.Dispatch(context.Background(), DispatchOptions{
		TicketRef:       task.Ref,
		TaskID:          task.ID,
		NoLaunch:        true,
		LeaseID:         "claim:67",
		LeaseGeneration: 3,
	})
	if err != nil {
		t.Fatalf("recovered generation-3 dispatch: %v", err)
	}

	gotContext, err := ReadTaskContext(wtInfo.Path)
	if err != nil {
		t.Fatalf("read generation-3 context: %v", err)
	}
	packetBytes, err := os.ReadFile(result.TaskPacket)
	if err != nil {
		t.Fatalf("read generation-3 packet: %v", err)
	}
	packet := string(packetBytes)

	for _, want := range []string{
		"task_ref: FAC-666",
		"task_id: task-666",
		"provider=kaneo project=project-666",
		"branch " + gotContext.Branch,
		`"base_sha": "` + gotContext.BaseSHA + `"`,
		`"session_id": "` + gotContext.SessionID + `"`,
		`"lease_id": "claim:67"`,
		"lease_generation: 3",
		"Completion callback: herd shot FAC-666 --report complete --sha <sha> --lease 3",
		"BLOCKED: herd shot FAC-666 --report blocked --detail \"<why>\" --lease 3",
	} {
		if !strings.Contains(packet, want) {
			t.Errorf("generation-3 packet missing %q\n%s", want, packet)
		}
	}
	for _, stale := range []string{"lease_generation: 2", "--lease 2"} {
		if strings.Contains(packet, stale) {
			t.Errorf("generation-3 packet retained stale field %q\n%s", stale, packet)
		}
	}
	if gotContext.TaskRef != task.Ref || gotContext.TaskID != task.ID ||
		gotContext.ProjectID != cfg.TaskProvider.ProjectID || gotContext.Branch != wtInfo.Branch ||
		gotContext.BaseSHA != wtInfo.BaseSHA || gotContext.LeaseID != "claim:67" ||
		gotContext.LeaseGeneration != 3 {
		t.Fatalf("generation-3 context binding mismatch: %+v", gotContext)
	}
	status, err := exec.Command("git", "-C", wtInfo.Path, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("recovered worktree status: %v", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		t.Fatalf("runtime task artifacts dirtied recovered worktree: %s", status)
	}
}

func TestTaskArtifactPublisherFailureBoundariesFailClosed(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	phases := []taskArtifactPhase{
		taskArtifactBeforePublish,
		taskArtifactContextTempCreate, taskArtifactContextTempWrite, taskArtifactContextTempSync, taskArtifactContextTempClose,
		taskArtifactPacketTempCreate, taskArtifactPacketTempWrite, taskArtifactPacketTempSync, taskArtifactPacketTempClose,
		taskArtifactReceiptTempCreate, taskArtifactReceiptTempWrite, taskArtifactReceiptTempSync, taskArtifactReceiptTempClose,
		taskArtifactInvalidateReceipt,
		taskArtifactContextRename, taskArtifactContextDirSync, taskArtifactContextReadback,
		taskArtifactPacketRename, taskArtifactPacketDirSync, taskArtifactPacketReadback,
		taskArtifactReceiptRename, taskArtifactReceiptDirSync, taskArtifactReceiptReadback,
		taskArtifactFinalValidation,
	}

	for _, phase := range phases {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			oldContext := artifactContext(t, signer, 2, "claim:66")
			oldPacket, err := (taskArtifactPublisher{}).Publish(dir, artifactPacket(oldContext), oldContext)
			if err != nil {
				t.Fatalf("seed generation 2: %v", err)
			}
			newContext := artifactContext(t, signer, 3, "claim:67")
			newPacket, err := bindTaskPacket(artifactPacket(newContext), newContext)
			if err != nil {
				t.Fatal(err)
			}
			publisher := taskArtifactPublisher{fail: func(got taskArtifactPhase) error {
				if got == phase {
					return errors.New("injected boundary failure")
				}
				return nil
			}}
			if _, err := publisher.Publish(dir, artifactPacket(newContext), newContext); err == nil || !strings.Contains(err.Error(), string(phase)) {
				t.Fatalf("phase %s did not fail closed: %v", phase, err)
			}
			if err := validateTaskArtifacts(dir, newPacket, newContext, verifier); err == nil {
				t.Fatalf("phase %s left incoming generation usable", phase)
			}
			if err := validateTaskArtifacts(dir, oldPacket, oldContext, verifier); err == nil {
				t.Fatalf("phase %s left prior generation usable", phase)
			}
			for _, name := range []string{TaskContextFile, TaskPacketFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Fatalf("phase %s erased attributable %s evidence: %v", phase, name, err)
				}
			}
			if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.Contains(entry.Name(), ".tmp-") {
					return fmt.Errorf("phase %s left temp artifact %s", phase, path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDispatchTaskArtifactFailureCompensatesAndReleasesLease(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	task := baseTask("FAC-666")
	tp := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{task}}}
	wtInfo, err := wm.CreateTaskWorktreeFrom(context.Background(), task.Ref, "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wtInfo.Path).Run()
		_ = os.RemoveAll(wtInfo.Path)
	})

	comp := &recordingCompensator{}
	ownership := &releaseTrackingOwnership{generation: 2}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Worktree = recoveredWorktree{root: repo, info: wtInfo}
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}
	d.Ownership = ownership
	d.taskArtifacts = &taskArtifactPublisher{fail: func(phase taskArtifactPhase) error {
		if phase == taskArtifactPacketReadback {
			return errors.New("injected packet readback failure")
		}
		return nil
	}}

	_, err = d.Dispatch(context.Background(), DispatchOptions{
		TicketRef: task.Ref, TaskID: task.ID, NoLaunch: true,
		LeaseID: "claim:67", LeaseGeneration: 3,
	})
	if err == nil || !strings.Contains(err.Error(), string(taskArtifactPacketReadback)) {
		t.Fatalf("dispatch artifact failure = %v", err)
	}
	if !hasCompensateReason(comp.compsCopy(), "task_artifact_publish_failed") {
		t.Fatalf("artifact failure was not durably compensated: %v", comp.compsCopy())
	}
	if ownership.releaseCount() != 1 {
		t.Fatalf("ownership releases = %d, want 1", ownership.releaseCount())
	}
	if err := validateTaskArtifacts(wtInfo.Path, "", TaskContext{}, nil); err == nil {
		t.Fatal("failed dispatch left a production-usable artifact pair")
	}
}

func TestTaskArtifactPublisherRetryIdempotenceAndPartialRepair(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	dir := t.TempDir()
	tc := artifactContext(t, signer, 3, "claim:67")
	packet := artifactPacket(tc)
	renames := 0
	publisher := taskArtifactPublisher{fail: func(phase taskArtifactPhase) error {
		if phase == taskArtifactReceiptRename {
			renames++
		}
		return nil
	}}
	first, err := publisher.Publish(dir, packet, tc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := publisher.Publish(dir, packet, tc)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || renames != 1 {
		t.Fatalf("idempotent retry first==second %v receipt renames=%d", first == second, renames)
	}
	if err := os.WriteFile(filepath.Join(dir, TaskPacketFile), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateTaskArtifacts(dir, first, tc, verifier); err == nil {
		t.Fatal("partial packet mutation passed exact digest validation")
	}
	repaired, err := publisher.Publish(dir, packet, tc)
	if err != nil {
		t.Fatalf("idempotent partial repair: %v", err)
	}
	if repaired != first || renames != 2 {
		t.Fatalf("partial repair changed payload=%v receipt renames=%d", repaired != first, renames)
	}
	if err := validateTaskArtifacts(dir, repaired, tc, verifier); err != nil {
		t.Fatalf("repaired pair invalid: %v", err)
	}
}

func TestValidateGeneratedPacketFieldsRejectsDuplicateStaleCallback(t *testing.T) {
	signer, _ := testSignerVerifier(t)
	tc := artifactContext(t, signer, 3, "claim:67")
	packet, err := bindTaskPacket(artifactPacket(tc), tc)
	if err != nil {
		t.Fatal(err)
	}
	stale := "\n  Completion callback: herd shot FAC-666 --report complete --sha <sha> --lease 2\n"
	if err := validateGeneratedPacketFields(packet+stale, tc); err == nil {
		t.Fatal("duplicate stale callback passed generated-field validation")
	}
}

func TestTaskArtifactPublisherConcurrentGenerationHasOneWinner(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	dir := t.TempDir()
	oldContext := artifactContext(t, signer, 2, "claim:66")
	if _, err := (taskArtifactPublisher{}).Publish(dir, artifactPacket(oldContext), oldContext); err != nil {
		t.Fatal(err)
	}
	contexts := []TaskContext{
		artifactContext(t, signer, 3, "claim:67-a"),
		artifactContext(t, signer, 3, "claim:67-b"),
	}
	type result struct {
		packet string
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, len(contexts))
	for _, tc := range contexts {
		tc := tc
		go func() {
			<-start
			packet, err := (taskArtifactPublisher{}).Publish(dir, artifactPacket(tc), tc)
			results <- result{packet: packet, err: err}
		}()
	}
	close(start)

	winner := -1
	conflicts := 0
	for range contexts {
		got := <-results
		if got.err == nil {
			for i, tc := range contexts {
				if binding, err := parseTaskPacketBinding(got.packet); err == nil && binding.SessionID == tc.SessionID {
					winner = i
				}
			}
			continue
		}
		if errors.Is(got.err, errTaskArtifactConflict) {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent publish error: %v", got.err)
	}
	if winner < 0 || conflicts != 1 {
		t.Fatalf("winner=%d conflicts=%d, want one each", winner, conflicts)
	}
	bound, err := bindTaskPacket(artifactPacket(contexts[winner]), contexts[winner])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTaskArtifacts(dir, bound, contexts[winner], verifier); err != nil {
		t.Fatalf("winning pair invalid: %v", err)
	}
	if _, err := (taskArtifactPublisher{}).Publish(dir, artifactPacket(oldContext), oldContext); !errors.Is(err, errTaskArtifactStale) {
		t.Fatalf("stale generation publish = %v, want ErrTaskArtifactStale", err)
	}
}

func TestProductionLaunchRejectsMutatedTaskArtifactPairBeforeHerdr(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	d := NewProductionDispatcher(testCfg(), nil, wm)
	fh := &fakeHerdr{available: true, workspace: "workspace", tabID: "must-not-open"}
	d.Herdr = fh
	d.Compensator = &recordingCompensator{}

	task := baseTask("FAC-666")
	lane := &d.Config.Lanes[0]
	wtInfo := &worktree.WorktreeInfo{
		Path: filepath.Join(repo, "isolated-fac-666"), Branch: "herd/fac-666",
		BaseSHA: "base-666", AnchorRef: worktree.AnchorRefFor(task.Ref),
	}
	if err := os.MkdirAll(wtInfo.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := DispatchOptions{TicketRef: task.Ref, LeaseID: "claim:67", LeaseGeneration: 3}
	tc, err := d.signReceipt(d.taskContext(task, wtInfo, wtInfo.Branch, lane, opts))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := (taskArtifactPublisher{}).Publish(wtInfo.Path, artifactPacket(tc), tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtInfo.Path, TaskPacketFile), []byte(packet+"mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := testRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort,
		TaskRef: task.Ref, LeaseGeneration: 3, Scope: router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.Decision = decision
	result := &DispatchResult{LeaseGeneration: 3}
	err = d.launch(context.Background(), opts, task, lane, wtInfo, wtInfo.Branch, packet, result, nil, tc)
	var launchErr *launchFailure
	if !errors.As(err, &launchErr) || launchErr.Reason != "task_artifact_mismatch" {
		t.Fatalf("mutated pair launch error = %T %v", err, err)
	}
	if fh.tabCwd != "" || fh.startCalls != 0 {
		t.Fatalf("mutated pair reached Herdr: cwd=%q starts=%d", fh.tabCwd, fh.startCalls)
	}
}
