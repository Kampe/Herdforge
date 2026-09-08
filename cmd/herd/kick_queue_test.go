package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
)

type allowKickHolds struct{}

func (allowKickHolds) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Generation: 1}, nil
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
