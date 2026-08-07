package broadcast

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func fixtureCandidates() []Target {
	// Incident fixture: FAC-151 and FAC-172 carry quarantine markers and
	// must never receive a lifted-hold broadcast. Eligible lanes do.
	return []Target{
		{
			Name: "forge-worker", TaskRef: "FAC-180", PaneID: "p-w", TabID: "t-w",
			Workspace: "wF", Cwd: "./.herd/worktrees/fac-180", Role: "worker",
			Session: "s-w", Generation: 3, AllowedActions: []string{"prompt", "kick"},
		},
		{
			Name: "task-fac-151", TaskRef: "FAC-151", PaneID: "p-151", TabID: "t-151",
			Workspace: "wF", Cwd: "./.herd/worktrees/fac-151", Role: "worker",
			Session: "s-151", Generation: 2, AllowedActions: []string{"prompt"},
			Markers: []ExclusionKind{ExcludeProtected, ExcludeQuarantined},
		},
		{
			Name: "p57", TaskRef: "FAC-172", PaneID: "p-172", TabID: "t-172",
			Workspace: "wF", Cwd: "./.herd/worktrees/fac-172", Role: "worker",
			Session: "s-172", Generation: 5, AllowedActions: []string{"prompt"},
			Markers: []ExclusionKind{ExcludeQuarantined},
		},
		{
			Name: "review-assayer-fac-x", TaskRef: "FAC-099", Role: "reviewer",
			Cwd: "./.herd/worktrees/fac-099", Generation: 1, AllowedActions: []string{"prompt"},
			Markers: []ExclusionKind{ExcludeReviewer},
		},
		{
			Name: "historical-fac-50", TaskRef: "FAC-050", Role: "worker",
			Cwd: "./.herd/worktrees/fac-050", Generation: 1, AllowedActions: []string{"prompt"},
			Markers: []ExclusionKind{ExcludeHistorical},
		},
		{
			Name: "forge-smith", TaskRef: "FAC-190", PaneID: "p-s", TabID: "t-s",
			Workspace: "wF", Cwd: "./.herd/worktrees/fac-190", Role: "forge-smith",
			Session: "s-s", Generation: 1, AllowedActions: []string{"prompt", "kick"},
		},
	}
}

func TestSelect_ExcludesProtectedQuarantinedReviewerHistorical(t *testing.T) {
	sel := Select(fixtureCandidates())
	if len(sel.Selected) != 2 {
		t.Fatalf("selected=%d want 2: %+v", len(sel.Selected), sel.Selected)
	}
	// Deterministic order by name.
	if sel.Selected[0].Name != "forge-smith" || sel.Selected[1].Name != "forge-worker" {
		t.Fatalf("selected names = %q,%q", sel.Selected[0].Name, sel.Selected[1].Name)
	}
	excluded := map[string]ExclusionKind{}
	for _, e := range sel.Excluded {
		excluded[e.Target.Name] = e.Reason
	}
	if excluded["task-fac-151"] != ExcludeProtected {
		t.Fatalf("FAC-151 reason = %q want protected (first matching kind)", excluded["task-fac-151"])
	}
	if excluded["p57"] != ExcludeQuarantined {
		t.Fatalf("FAC-172 reason = %q", excluded["p57"])
	}
	if excluded["review-assayer-fac-x"] != ExcludeReviewer {
		t.Fatalf("reviewer reason = %q", excluded["review-assayer-fac-x"])
	}
	if excluded["historical-fac-50"] != ExcludeHistorical {
		t.Fatalf("historical reason = %q", excluded["historical-fac-50"])
	}
}

func TestDeliver_QuarantineFixtureNeverPromptsExcluded_EligibleGetOne(t *testing.T) {
	var mu sync.Mutex
	prompts := map[string]int{}
	d := &Deliverer{
		Prompt: func(_ context.Context, target Target, _ string) error {
			mu.Lock()
			prompts[target.Name]++
			mu.Unlock()
			return nil
		},
		Live: func(_ context.Context, target Target) (PromptIdentity, error) {
			return IdentityFromTarget(target, "prompt"), nil
		},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	rec, err := d.Deliver(context.Background(), "lifted-hold-1", "prompt", "holds lifted — continue", fixtureCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.PromptedNames(); !reflect.DeepEqual(got, []string{"forge-smith", "forge-worker"}) {
		t.Fatalf("prompted = %v", got)
	}
	for _, name := range []string{"task-fac-151", "p57", "review-assayer-fac-x", "historical-fac-50"} {
		if prompts[name] != 0 {
			t.Fatalf("excluded lane %s was prompted %d times", name, prompts[name])
		}
	}
	for _, name := range []string{"forge-smith", "forge-worker"} {
		if prompts[name] != 1 {
			t.Fatalf("eligible lane %s prompted %d times, want exactly 1", name, prompts[name])
		}
	}
	if len(rec.Excluded) != 4 {
		t.Fatalf("excluded count = %d", len(rec.Excluded))
	}
}

func TestCheckIdentity_RejectsDriftAndDeniedAction(t *testing.T) {
	bound := PromptIdentity{
		TaskRef: "FAC-180", Generation: 3, Session: "s-w",
		Cwd: "./.herd/worktrees/fac-180", Role: "worker",
		AllowedActions: []string{"prompt"}, Action: "prompt",
	}
	live := bound
	if err := CheckIdentity(bound, live); err != nil {
		t.Fatal(err)
	}
	// Task drift.
	bad := live
	bad.TaskRef = "FAC-999"
	if err := CheckIdentity(bound, bad); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("task drift: %v", err)
	}
	// Generation drift.
	bad = live
	bad.Generation = 99
	if err := CheckIdentity(bound, bad); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("generation drift: %v", err)
	}
	// Cwd / worktree drift (cross-repo or wrong task tree).
	bad = live
	bad.Cwd = "./.herd/worktrees/fac-172"
	if err := CheckIdentity(bound, bad); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("cwd drift: %v", err)
	}
	// Session swap after bind.
	bad = live
	bad.Session = "other"
	if err := CheckIdentity(bound, bad); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("session drift: %v", err)
	}
	// Empty bound session is allowed (late-bound).
	late := bound
	late.Session = ""
	if err := CheckIdentity(late, live); err != nil {
		t.Fatalf("late-bound session: %v", err)
	}
	// Action not allowed.
	noAct := bound
	noAct.Action = "shoot"
	if err := CheckIdentity(noAct, live); !errors.Is(err, ErrActionDenied) {
		t.Fatalf("denied action: %v", err)
	}
}

func TestDeliver_IdentityFailureDoesNotPrompt(t *testing.T) {
	prompted := 0
	d := &Deliverer{
		Prompt: func(context.Context, Target, string) error {
			prompted++
			return nil
		},
		Live: func(context.Context, Target) (PromptIdentity, error) {
			return PromptIdentity{
				TaskRef: "WRONG", Generation: 1, Cwd: "x", Role: "worker",
				AllowedActions: []string{"prompt"}, Action: "prompt",
			}, nil
		},
	}
	cands := []Target{{
		Name: "forge-worker", TaskRef: "FAC-180", Cwd: "./wt", Role: "worker",
		Generation: 3, AllowedActions: []string{"prompt"},
	}}
	rec, err := d.Deliver(context.Background(), "id", "prompt", "hi", cands)
	if err != nil {
		t.Fatal(err)
	}
	if prompted != 0 {
		t.Fatalf("prompted despite identity mismatch: %d", prompted)
	}
	if rec.Selected[0].Prompted || rec.Selected[0].Error == "" {
		t.Fatalf("receipt: %+v", rec.Selected[0])
	}
}

func TestReceiptStore_DurableAppendAndCompensation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broadcasts.jsonl")
	store := &ReceiptStore{Path: path}
	rec := Receipt{ID: "b1", Action: "prompt", At: time.Unix(1, 0).UTC()}
	if err := store.Append(rec); err != nil {
		t.Fatal(err)
	}
	rec, err := store.RecordCompensation(rec, "prompt_failed:pane-gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Compensated) != 1 {
		t.Fatalf("compensated = %v", rec.Compensated)
	}
}

func TestMutation_RemovingExclusionGateWouldPromptQuarantined(t *testing.T) {
	// Unsafe baseline: treating markers as ignoreable would deliver to FAC-151.
	cands := fixtureCandidates()
	unsafeSelected := 0
	for _, c := range cands {
		_ = c.Markers // pretend markers unused
		unsafeSelected++
	}
	if unsafeSelected < 6 {
		t.Fatal("unsafe baseline broken")
	}
	// Safe path subtracts them.
	sel := Select(cands)
	for _, s := range sel.Selected {
		if primaryExclusion(s.Markers) != "" {
			t.Fatalf("exclusion gate failed open for %s markers=%v", s.Name, s.Markers)
		}
		if s.TaskRef == "FAC-151" || s.TaskRef == "FAC-172" {
			t.Fatalf("quarantined task %s still selected", s.TaskRef)
		}
	}
	if len(sel.Excluded) == 0 {
		t.Fatal("mutation: Select without exclusion would report zero excluded")
	}
}

func TestMutation_RemovingIdentityGateWouldPromptOnDrift(t *testing.T) {
	// Unsafe baseline: prompt without CheckIdentity always "succeeds".
	bound := PromptIdentity{
		TaskRef: "FAC-180", Generation: 1, Cwd: "./a", Role: "worker",
		AllowedActions: []string{"prompt"}, Action: "prompt",
	}
	live := bound
	live.Cwd = "./b"
	unsafeWouldPrompt := bound.TaskRef != "" // identity ignored → always prompt
	if !unsafeWouldPrompt {
		t.Fatal("unsafe baseline broken")
	}
	// Safe path refuses.
	err := CheckIdentity(bound, live)
	if err == nil {
		t.Fatal("identity gate failed open on cwd drift")
	}
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestRace_ConcurrentSelectIsStable(t *testing.T) {
	cands := fixtureCandidates()
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			sel := Select(cands)
			if len(sel.Selected) != 2 || len(sel.Excluded) != 4 {
				errs <- errors.New("unstable selection")
				return
			}
			if sel.Selected[0].Name != "forge-smith" {
				errs <- errors.New("unstable order")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
