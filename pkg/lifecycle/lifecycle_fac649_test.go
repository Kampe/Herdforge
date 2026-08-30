package lifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustParseBoardCards(t testing.TB, payload json.RawMessage) []boardCard {
	t.Helper()
	cards, err := (&Engine{}).parseCards(payload)
	if err != nil {
		t.Fatalf("parse stock task fixture: %v", err)
	}
	return cards
}

func writeLifecycleExecutable(t testing.TB, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture-command")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeLifecycleAgents(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(`{"result":{"agents":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestActModeRequiresApprovedReclaimHookBeforeBoardWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setHook  bool
		approved bool
	}{
		{name: "absent"},
		{name: "unapproved", setHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "board-writes")
			t.Setenv("FAC649_BOARD_WRITE_LOG", logPath)
			command := writeLifecycleExecutable(t, `printf '%s\n' "$*" >> "$FAC649_BOARD_WRITE_LOG"`)
			e := &Engine{KaneoBin: command}
			if tc.setHook {
				e.ReclaimHook = command
			}
			if tc.approved {
				e.ApprovedReclaimHooks = []string{command}
			}
			summary := &Summary{
				StaleInProgress: 1,
				StaleCards: []StaleCard{{
					Ref:   "FAC-1",
					Owner: "owner-1",
					Role:  "worker",
				}},
			}
			err := e.executeActMode(t.TempDir(), t.TempDir(), summary, nil, nil, nil, nil)
			if !errors.Is(err, ErrReclaimHookRequired) {
				t.Fatalf("reclaim hook guard error = %v, want %v", err, ErrReclaimHookRequired)
			}
			if data, readErr := os.ReadFile(logPath); readErr == nil {
				t.Fatalf("board command ran before reclaim-hook guard: %q", data)
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("read board-command log: %v", readErr)
			}
		})
	}
}

func TestStockKaneoTaskSchemaPreservesExactFieldsAndRole(t *testing.T) {
	payload := json.RawMessage(`[
		{"id":"task-9","ref":"FAC-009","title":"Exact","status":"In-Progress","userId":"Owner-A","labels":[{"name":"Worker"},"Urgent"],"createdAt":"2026-08-30T01:02:03Z","updatedAt":"2026-08-30T04:05:06Z"}
	]`)
	cards := mustParseBoardCards(t, payload)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	card := cards[0]
	if card.ID != "task-9" || card.Ref != "FAC-009" || card.Status != "In-Progress" || card.UserID != "Owner-A" {
		t.Fatalf("stock identity/status/owner changed: %+v", card)
	}
	if !slices.Equal([]string(card.Labels), []string{"Worker", "Urgent"}) {
		t.Fatalf("stock labels = %q", card.Labels)
	}
	if card.CreatedAt != "2026-08-30T01:02:03Z" || card.UpdatedAt != "2026-08-30T04:05:06Z" {
		t.Fatalf("stock timestamps changed: %+v", card)
	}

	engine := &Engine{HoldRoles: []string{"worker"}}
	agents := struct {
		Result struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"result"`
	}{}
	summary := engine.computeSummary(agents, cards, nil, nil)
	if len(summary.StaleCards) != 1 {
		t.Fatalf("stale cards = %+v, want one", summary.StaleCards)
	}
	stale := summary.StaleCards[0]
	if stale.Ref != "FAC-009" || stale.Owner != "Owner-A" || stale.Role != "worker" {
		t.Fatalf("stale identity/role changed: %+v", stale)
	}
}

func TestStockKaneoTaskListRejectsInvalidPayloads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{name: "error envelope", payload: `{"error":"provider unavailable"}`, want: "error envelope"},
		{name: "errors envelope", payload: `{"errors":[{"message":"provider unavailable"}]}`, want: "errors envelope"},
		{name: "malformed", payload: `[{"id":`, want: "decode stock Kaneo task list"},
		{name: "missing id", payload: `[{"ref":"FAC-1","status":"to-do"}]`, want: "missing id or ref"},
		{name: "missing ref", payload: `[{"id":"task-1","status":"to-do"}]`, want: "missing id or ref"},
		{name: "missing status", payload: `[{"id":"task-1","ref":"FAC-1"}]`, want: "missing status"},
		{name: "retired wrapper", payload: `{"tasks":[]}`, want: "must be a JSON array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Engine{}).parseCards(json.RawMessage(tc.payload))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestObserveRejectsErrorEnvelopeFromSuccessfulKaneoCommand(t *testing.T) {
	kaneo := writeLifecycleExecutable(t, `printf '%s\n' '{"error":"provider unavailable"}'`)
	engine := &Engine{AgentsFile: writeLifecycleAgents(t), KaneoBin: kaneo}
	if _, err := engine.Point(); !errors.Is(err, ErrBoardObservation) {
		t.Fatalf("Point error = %v, want %v", err, ErrBoardObservation)
	}
}

func TestObserveStockKaneoProviderTimeoutFailsClosed(t *testing.T) {
	kaneo := writeLifecycleExecutable(t, `exec sleep 1`)
	engine := &Engine{
		AgentsFile:       writeLifecycleAgents(t),
		KaneoBin:         kaneo,
		BoardReadTimeout: 20 * time.Millisecond,
	}
	if _, err := engine.Point(); !errors.Is(err, ErrBoardObservation) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Point timeout error = %v, want board-observation timeout", err)
	}
}

func TestObserveUsesExactStockKaneoListCommand(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "list-args")
	t.Setenv("FAC649_LIST_LOG", logPath)
	kaneo := writeLifecycleExecutable(t, `printf '%s\n' "$*" > "$FAC649_LIST_LOG"
printf '%s\n' '[]'`)
	engine := &Engine{AgentsFile: writeLifecycleAgents(t), KaneoBin: kaneo}
	if _, err := engine.Point(); err != nil {
		t.Fatalf("Point: %v", err)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "task list --json --all\n" {
		t.Fatalf("stock Kaneo arguments = %q", args)
	}
}

func TestApprovedReclaimHookUsesExplicitRefAndExactReadback(t *testing.T) {
	hookLog := filepath.Join(t.TempDir(), "hook-args")
	t.Setenv("FAC649_HOOK_LOG", hookLog)
	hook := writeLifecycleExecutable(t, `printf '%s\n' "$*" > "$FAC649_HOOK_LOG"`)
	readback := writeLifecycleExecutable(t, `printf '%s\n' '{"id":"task-1","ref":"FAC-1","status":"to-do","userId":"","labels":[]}'`)
	engine := &Engine{
		ReclaimHook:          hook,
		ApprovedReclaimHooks: []string{hook},
		ReadbackHook:         readback,
	}
	summary := &Summary{
		StaleInProgress: 1,
		StaleCards: []StaleCard{{
			Ref:   "FAC-1",
			Owner: "Owner-A",
			Role:  "worker",
		}},
	}
	if err := engine.executeActMode(t.TempDir(), t.TempDir(), summary, nil, nil, nil, nil); err != nil {
		t.Fatalf("execute approved reclaim: %v", err)
	}
	invocation, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "--ref FAC-1 --owner Owner-A --reason stale in-progress owner --return-to-to-do\n"
	if string(invocation) != want || strings.Contains(string(invocation), "FAC-2") {
		t.Fatalf("reclaim invocation = %q, want %q", invocation, want)
	}
	if summary.StaleActionsExecuted != 1 || len(summary.Actions) != 1 || !summary.Actions[0].Verified {
		t.Fatalf("verified action summary = %+v", summary)
	}
}

func TestApprovedReclaimHookAndReadbackFailuresAreHard(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hookBody     string
		readbackBody string
		want         error
	}{
		{name: "hook error", hookBody: `exit 19`, readbackBody: `printf '%s\n' '{}'`},
		{name: "error envelope", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"error":"read unavailable"}'`, want: ErrReclaimReadback},
		{name: "malformed", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"id":'`, want: ErrReclaimReadback},
		{name: "missing identity", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"status":"to-do"}'`, want: ErrReclaimReadback},
		{name: "wrong ref", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"id":"task-2","ref":"FAC-2","status":"to-do","userId":""}'`, want: ErrReclaimReadback},
		{name: "wrong status", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"id":"task-1","ref":"FAC-1","status":"in-progress","userId":""}'`, want: ErrReclaimReadback},
		{name: "still assigned", hookBody: `exit 0`, readbackBody: `printf '%s\n' '{"id":"task-1","ref":"FAC-1","status":"to-do","userId":"Owner-A"}'`, want: ErrReclaimReadback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := writeLifecycleExecutable(t, tc.hookBody)
			readback := writeLifecycleExecutable(t, tc.readbackBody)
			engine := &Engine{
				ReclaimHook:          hook,
				ApprovedReclaimHooks: []string{hook},
				ReadbackHook:         readback,
			}
			summary := &Summary{
				StaleInProgress: 1,
				StaleCards:      []StaleCard{{Ref: "FAC-1", Owner: "Owner-A", Role: "worker"}},
			}
			err := engine.executeActMode(t.TempDir(), t.TempDir(), summary, nil, nil, nil, nil)
			if err == nil {
				t.Fatal("expected hard failure")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if summary.StaleActionsExecuted != 0 || len(summary.Actions) != 0 {
				t.Fatalf("failed reclaim recorded verified action: %+v", summary)
			}
		})
	}
}
