package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestSessionCompatible_EmptyBoundAllowsLiveFill(t *testing.T) {
	// Production: bind right after AgentStart (session ""), wake after boot (UUID).
	if !SessionCompatible("", "82458414-3fad-47eb-9c16-7006ac614bfa") {
		t.Fatal("empty→filled must be compatible (claude/opencode boot)")
	}
	if !SessionCompatible("", "") {
		t.Fatal("empty→empty must be compatible (grok)")
	}
	if !SessionCompatible("sess-a", "sess-a") {
		t.Fatal("same session must match")
	}
	if SessionCompatible("sess-a", "sess-b") {
		t.Fatal("session swap must be drift")
	}
	if SessionCompatible("sess-a", "") {
		t.Fatal("filled→empty must be drift (session disappeared)")
	}
}

func TestWakeTargetsCompatible_EmptyToFilledSession(t *testing.T) {
	bound := WakeTarget{
		Target: "task-fac-185", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1",
		AgentName: "task-fac-185", Provider: "claude", LeaseGeneration: 2,
		SessionID: "", // launch-time bind
	}
	live := bound
	live.SessionID = "sess-after-boot"
	if !WakeTargetsCompatible(bound, live) {
		t.Fatal("empty→filled session must not look like a different agent")
	}
	live.PaneID = "wK:p-other"
	if WakeTargetsCompatible(bound, live) {
		t.Fatal("pane recycle must still be drift")
	}
}

func TestHerdrWaker_EmptyToFilledSessionDoesNotStale(t *testing.T) {
	prompted := false
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt-delivery" {
			t.Fatalf("must not invent prompt-delivery: %v", args)
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			st := "idle"
			if prompted {
				st = "working"
			}
			// Live has a session UUID the bound target never had.
			return `{"result":{"agents":[{"name":"task-x","agent":"claude","agent_status":"` + st + `","pane_id":"wK:p1","tab_id":"wK:t1","workspace_id":"wK","agent_session":{"value":"sess-live","source":"herdr:claude","kind":"id"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			// Must address by pane id when present.
			if args[2] != "wK:p1" {
				t.Fatalf("expected pane-id prompt target, got %v", args)
			}
			prompted = true
			return "ok", nil
		}
		return "", nil
	})
	defer restore()

	bound := WakeTarget{
		Target: "task-x", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1",
		AgentName: "task-x", Provider: "claude", LeaseGeneration: 1,
		SessionID: "", // never filled at bind
	}
	w := HerdrWaker{
		Target: bound,
		Validate: func(_ context.Context, got WakeTarget) (WakeTarget, error) {
			// Production validate fills session from live list.
			got.SessionID = "sess-live"
			return got, nil
		},
	}
	// ReadTarget must not ErrStaleIdentity on empty→filled.
	if _, err := w.ReadTarget(context.Background()); err != nil {
		t.Fatalf("ReadTarget: %v", err)
	}
	rec, err := w.Wake(context.Background(), WakeRequest{
		MessageID: "control-abc", Sequence: 1, Target: bound,
	})
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if !rec.Consumed || rec.SessionID != "sess-live" {
		t.Fatalf("receipt: %+v", rec)
	}
}

func TestHerdrWaker_SessionSwapIsStale(t *testing.T) {
	bound := WakeTarget{
		Target: "task-x", Workspace: "wK", TabID: "wK:t1", PaneID: "wK:p1",
		AgentName: "task-x", Provider: "claude", LeaseGeneration: 1,
		SessionID: "sess-original",
	}
	w := HerdrWaker{
		Target: bound,
		Validate: func(_ context.Context, got WakeTarget) (WakeTarget, error) {
			got.SessionID = "sess-recycled"
			return got, nil
		},
	}
	_, err := w.Wake(context.Background(), WakeRequest{MessageID: "m", Sequence: 1, Target: bound})
	if !errors.Is(err, ErrStaleIdentity) {
		t.Fatalf("expected ErrStaleIdentity on session swap, got %v", err)
	}
}

func TestHerdrWaker_GrokNeverHasSession(t *testing.T) {
	prompted := false
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			st := "idle"
			if prompted {
				st = "working"
			}
			return `{"result":{"agents":[{"name":"task-g","agent":"grok","agent_status":"` + st + `","pane_id":"p9","tab_id":"t9","workspace_id":"wK"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
			return "ok", nil
		}
		return "", nil
	})
	defer restore()

	bound := WakeTarget{
		Target: "task-g", Workspace: "wK", TabID: "t9", PaneID: "p9",
		AgentName: "task-g", Provider: "grok", LeaseGeneration: 2,
	}
	w := HerdrWaker{
		Target:   bound,
		Timeout:  3 * time.Second,
		Validate: func(_ context.Context, g WakeTarget) (WakeTarget, error) { return g, nil },
	}
	rec, err := w.Wake(context.Background(), WakeRequest{MessageID: "m", Sequence: 1, Target: bound})
	if err != nil {
		t.Fatalf("grok wake: %v", err)
	}
	if !rec.Consumed || rec.SessionID != "" {
		t.Fatalf("receipt: %+v", rec)
	}
}

func TestPromptTargetPrefersPaneID(t *testing.T) {
	t1 := WakeTarget{Target: "task-x", PaneID: "wK:p1"}
	if t1.PromptTarget() != "wK:p1" {
		t.Fatalf("got %q", t1.PromptTarget())
	}
	t2 := WakeTarget{Target: "task-x"}
	if t2.PromptTarget() != "task-x" {
		t.Fatalf("got %q", t2.PromptTarget())
	}
}

func TestRevalidatingAuthorityUsesOrderIdentityWithoutTabGeneration(t *testing.T) {
	order := Order{LaneIdentity: LaneIdentity{Repository: "repo", TaskRef: "FAC-304", Lane: "worker", LeaseGeneration: 7, CandidateSHA: "sha"}, Kind: KindRepair, Body: "repair"}
	called := false
	a := RevalidatingAuthority{Check: func(_ context.Context, got Order) error {
		called = true
		if got != order {
			t.Fatalf("check received %+v, want %+v", got, order)
		}
		return nil
	}}
	identity, err := a.Resolve(context.Background(), order)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !called || identity != order.LaneIdentity {
		t.Fatalf("identity=%+v called=%v", identity, called)
	}

	for _, want := range []error{ErrStaleIdentity, errors.New("claim unavailable")} {
		want := want
		t.Run(want.Error(), func(t *testing.T) {
			got, err := (RevalidatingAuthority{Check: func(context.Context, Order) error { return want }}).Resolve(context.Background(), order)
			if !errors.Is(err, want) || got != (LaneIdentity{}) {
				t.Fatalf("got identity=%+v err=%v", got, err)
			}
		})
	}
}

func TestHerdrWaker_NeverCallsPromptDelivery(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt-delivery" {
			t.Fatalf("invented subcommand: %v", args)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "prompt-delivery") {
			t.Fatalf("invented subcommand: %v", args)
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"n","agent":"codex","agent_status":"working","pane_id":"p","tab_id":"t","workspace_id":"w"}]}}`, nil
		}
		return "ok", nil
	})
	defer restore()
	// Only exercises Validate identity path with incomplete setup — ensure
	// incomplete target fails before any CLI call that invents APIs.
	w := HerdrWaker{Target: WakeTarget{Target: "n"}, Validate: func(_ context.Context, g WakeTarget) (WakeTarget, error) { return g, nil }}
	_, err := w.Wake(context.Background(), WakeRequest{Target: w.Target, MessageID: "m", Sequence: 1})
	if err == nil {
		t.Fatal("expected incomplete identity error")
	}
}
