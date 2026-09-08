package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
)

type allowKickHolds struct{}

func (allowKickHolds) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Generation: 1}, nil
}

func TestWorktreeDefaultQueueReachesProductionKickSurface(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	lane := filepath.Join(t.TempDir(), "lane-wt")
	if out, err := testgit.Command(repo, "worktree", "add", "-q", "-b", "fac773-lane", lane).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}
	bin, logPath, statusPath := installQueuedSendFake(t, "working", strconv.Itoa(proc.Pid))
	t.Setenv("HERD_ROOT", lane)
	t.Setenv("HERD_PROJECT_ROOT", "")
	t.Setenv("HERD_MAIL_FILE", "")
	t.Setenv("HERD_WORKSPACE", "wK")
	t.Chdir(lane)

	body := "cross-worktree queued packet"
	got, err := herdr.Send("worker", body, false, time.Second)
	if err != nil || got != herdr.StatusQueuedDurable {
		t.Fatalf("default busy send = %q, %v", got, err)
	}
	log := fakeCallLog(t, logPath)
	for _, forbidden := range []string{"agent prompt", "send-keys", "Escape", "kill"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("busy default queue touched the pane (%q):\n%s", forbidden, log)
		}
	}

	if err := os.WriteFile(statusPath, []byte("idle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	occupied, surfErr := surfaceQueuedAtKick("worker", "wK")
	if surfErr != nil {
		t.Fatalf("production kick surface: %v", surfErr)
	}
	if !occupied {
		t.Fatalf("same envelope was not consumed at production kick surface; lane mailbox=%v project mailbox=%v log=%s",
			fileExists(t, filepath.Join(lane, ".herd", "control-mail.jsonl")),
			fileExists(t, filepath.Join(repo, ".herd", "control-mail.jsonl")),
			fakeCallLog(t, logPath))
	}
	projectBox := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl"))
	pending, err := projectBox.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("acked envelope still pending at project root: %+v", pending)
	}
	if _, err := os.Stat(filepath.Join(lane, ".herd", "control-mail.jsonl")); err == nil {
		t.Fatal("lane-local mailbox must not receive the project control envelope")
	}
	log = fakeCallLog(t, logPath)
	if !strings.Contains(log, "agent prompt") || !strings.Contains(log, body) {
		t.Fatalf("kick surface did not deliver to worker:\n%s", log)
	}
	if strings.Contains(log, "Escape") {
		t.Fatalf("routine surface sent Escape:\n%s", log)
	}
	_ = bin
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestKickRunSurfacesQueuedMailWithoutOperatorDrain(t *testing.T) {
	proc := startFakeCommand(t)
	repo := queuedSendRepo(t)
	bin, logPath, statusPath := installQueuedSendFake(t, "working", strconv.Itoa(proc.Pid))
	env := queuedSendEnv(bin, repo)
	body := "queued while working"
	out, err := runHerd(t, repo, env, "send", "worker", body, "--no-verify")
	if err != nil {
		t.Fatalf("queue: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "queued-durable") {
		t.Fatalf("want queued-durable, got %s", out)
	}

	if err := os.WriteFile(statusPath, []byte("idle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	box := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl"))
	sent := 0
	result, err := kick.Run(kick.Options{
		Names:        []string{"worker"},
		Quiet:        true,
		RaiseMissing: false,
		HoldReader:   allowKickHolds{},
		Identity: func(name string) (lifecycle.HoldIdentity, error) {
			return lifecycle.HoldIdentity{Repository: "repo", Owner: name, Lane: name, Scope: "lane"}, nil
		},
		ActiveTasks: func(_ context.Context, lane string) ([]lifecycle.HoldIdentity, error) {
			return []lifecycle.HoldIdentity{{Repository: "repo", Owner: lane, Lane: lane, Task: lane + "-task", Scope: "task"}}, nil
		},
		Generation: func(context.Context, lifecycle.HoldIdentity) (int64, error) { return 1, nil },
		Freeze:     func() (bool, string, error) { return false, "", nil },
		FetchAgents: func() ([]kick.AgentEntry, error) {
			return []kick.AgentEntry{{Name: "worker", Status: "idle", PaneID: "p1", Workspace: "wK"}}, nil
		},
		SurfaceQueued: func(name, workspace string) (bool, error) {
			return surfaceQueuedAtKickMailbox(name, workspace, box)
		},
		Send: func(paneID, message string) (string, error) {
			sent++
			return "", fmt.Errorf("kick message must not send: %s", message)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kicked != 1 || result.Failed != 0 || sent != 0 {
		t.Fatalf("kick=%+v sent=%d log=%s", result, sent, fakeCallLog(t, logPath))
	}
	log := fakeCallLog(t, logPath)
	if !strings.Contains(log, "agent prompt") || !strings.Contains(log, body) {
		t.Fatalf("idle kick did not surface queued mail:\n%s", log)
	}
	if strings.Contains(log, "Escape") {
		t.Fatalf("routine kick drain sent Escape:\n%s", log)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("queued mail stayed pending: %+v", pending)
	}
}
