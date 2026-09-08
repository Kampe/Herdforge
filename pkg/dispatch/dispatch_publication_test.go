package dispatch

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

const harvestNoPushBytes = "Do NOT push, PR, or merge — the coordinator harvests your branch. Do NOT touch the root checkout."

func completePublicationPolicy(mode string) *config.MergePolicy {
	return &config.MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{"Build"},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
		BranchPublication:            mode,
	}
}

func dispatchPublicationPacket(t *testing.T, mode, assignedBranch string) (packet string, err error) {
	t.Helper()
	repo, wm := initDispatchRepo(t)
	installTaskArtifactIgnores(t, repo)
	task := &provider.Task{
		ID:          "task-778",
		Ref:         "FAC-778",
		Title:       "Branch publication",
		Status:      provider.StatusToDo,
		Description: emptyDepsFence("FAC-778", "task-778"),
	}
	tp := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{task}}}
	cfg := testCfg()
	cfg.TaskProvider = config.TaskProvider{Type: "kaneo", ProjectID: "project-778"}
	if mode != "omit" {
		cfg.MergePolicy = completePublicationPolicy(mode)
	}

	wtInfo, err := wm.CreateTaskWorktreeFrom(context.Background(), task.Ref, "main")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wtInfo.Path).Run()
		_ = os.RemoveAll(wtInfo.Path)
	})
	if assignedBranch != "" {
		wtInfo.Branch = assignedBranch
	}

	d := NewDispatcher(cfg, tp, wm)
	d.Worktree = recoveredWorktree{root: repo, info: wtInfo}
	d.Compensator = &recordingCompensator{}
	d.Herdr = &fakeHerdr{available: false}

	result, err := d.Dispatch(context.Background(), DispatchOptions{
		TicketRef:       task.Ref,
		TaskID:          task.ID,
		NoLaunch:        true,
		LeaseID:         "claim:778",
		LeaseGeneration: 1,
	})
	if err != nil {
		return "", err
	}
	body, readErr := os.ReadFile(result.TaskPacket)
	if readErr != nil {
		t.Fatalf("read packet: %v", readErr)
	}
	return string(body), nil
}

func assertPacketContractPreserved(t *testing.T, packet string) {
	t.Helper()
	for _, want := range []string{
		"herd verify",
		"herd shot FAC-778 --report complete",
		"herd shot FAC-778 --report blocked",
		"READY-FOR-REVIEW",
		"review supervisor",
		"lease_generation: 1",
		"Do NOT touch the root checkout",
	} {
		if !strings.Contains(packet, want) {
			t.Errorf("packet lost required section %q:\n%s", want, packet)
		}
	}
}

func TestDispatchPacketOmittedBranchPublicationKeepsHarvestBytes(t *testing.T) {
	packet, err := dispatchPublicationPacket(t, "omit", "recovery/fac-778-branch-publication")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(packet, harvestNoPushBytes) {
		t.Fatalf("omitted branch_publication must keep today's no-push bytes:\n%s", packet)
	}
	if strings.Contains(packet, "git push -u origin") {
		t.Fatalf("omitted mode must not instruct a lane push:\n%s", packet)
	}
	if !strings.Contains(packet, "recovery/fac-778-branch-publication") {
		t.Fatalf("packet must still name the assigned branch:\n%s", packet)
	}
	if strings.Contains(packet, "herd/fac-778") {
		t.Fatalf("packet must not hardcode herd/<ref> when the assigned branch differs:\n%s", packet)
	}
	assertPacketContractPreserved(t, packet)
}

func TestDispatchPacketLanePushUsesAssignedBranchAndForbidsPRMerge(t *testing.T) {
	assigned := "recovery/fac-778-branch-publication"
	packet, err := dispatchPublicationPacket(t, config.BranchPublicationLanePush, assigned)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(packet, "git push -u origin "+assigned) {
		t.Fatalf("lane-push must instruct git push -u origin of the assigned branch:\n%s", packet)
	}
	if strings.Contains(packet, "git push -u origin herd/fac-778") {
		t.Fatalf("lane-push must not hardcode herd/<ref> when the assigned branch differs:\n%s", packet)
	}
	if strings.Contains(strings.ToLower(packet), "chainseer") {
		t.Fatalf("packet must not hardcode Chainseer:\n%s", packet)
	}
	if !strings.Contains(packet, assigned) {
		t.Fatalf("lane-push must name the assigned branch:\n%s", packet)
	}
	lower := strings.ToLower(packet)
	if !strings.Contains(lower, "remote") || !strings.Contains(lower, "head") || !strings.Contains(packet, "git rev-parse HEAD") {
		t.Fatalf("lane-push must require remote-head confirmation against committed HEAD:\n%s", packet)
	}
	if strings.Contains(packet, harvestNoPushBytes) {
		t.Fatalf("lane-push must drop the harvest-only no-push sentence:\n%s", packet)
	}
	if strings.Contains(lower, "do not push") {
		t.Fatalf("lane-push must not forbid the required push:\n%s", packet)
	}
	if !strings.Contains(packet, "Do NOT") || !(strings.Contains(packet, "PR") || strings.Contains(packet, "pr")) || !strings.Contains(lower, "merge") {
		t.Fatalf("lane-push must still forbid worker PR/merge:\n%s", packet)
	}
	assertPacketContractPreserved(t, packet)
}

func TestDispatchPacketCoordinatorHarvestMatchesOmittedBytes(t *testing.T) {
	packet, err := dispatchPublicationPacket(t, config.BranchPublicationCoordinatorHarvest, "feature/not-herd-ref")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(packet, harvestNoPushBytes) {
		t.Fatalf("coordinator-harvest must keep today's no-push bytes:\n%s", packet)
	}
	if strings.Contains(packet, "git push -u origin") {
		t.Fatalf("coordinator-harvest must not instruct a lane push:\n%s", packet)
	}
	if strings.Contains(packet, "herd/fac-778") {
		t.Fatalf("packet must not hardcode herd/<ref> when the assigned branch differs:\n%s", packet)
	}
	assertPacketContractPreserved(t, packet)
}

func TestDispatchRejectsInvalidBranchPublicationBeforeLaunch(t *testing.T) {
	_, err := dispatchPublicationPacket(t, "worker-push", "recovery/fac-778-branch-publication")
	if err == nil {
		t.Fatal("invalid branch_publication was allowed to dispatch")
	}
	if !strings.Contains(err.Error(), "branch_publication") {
		t.Fatalf("dispatch error must name branch_publication, got %v", err)
	}
}

func TestDispatchPacketLanePushEscapesUnsafeAssignedBranch(t *testing.T) {
	unsafe := "recovery/fac-778; rm -rf /"
	packet, err := dispatchPublicationPacket(t, config.BranchPublicationLanePush, unsafe)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(packet, "git push -u origin "+unsafe) {
		t.Fatalf("unsafe branch must not be interpolated unquoted:\n%s", packet)
	}
	pushIdx := strings.Index(packet, "git push -u origin ")
	if pushIdx < 0 {
		t.Fatalf("lane-push must instruct a push:\n%s", packet)
	}
	line := packet[pushIdx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	if strings.Contains(line, "project-778") {
		t.Fatalf("push command must not use the project slug as the branch: %q", line)
	}
	if strings.Contains(line, "herd/fac-778") {
		t.Fatalf("push command must not hardcode herd/<ref>: %q", line)
	}
	if !strings.Contains(line, "recovery/fac-778") {
		t.Fatalf("escaped push command must still name the assigned branch: %q", line)
	}
}
