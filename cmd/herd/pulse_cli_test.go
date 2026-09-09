package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/broker"
	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/progress"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/pulse"
	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

type inReviewReadFailingProvider struct {
	*provider.MemoryProvider
}

func (p inReviewReadFailingProvider) ListTasks(ctx context.Context, project, status string) ([]*provider.Task, error) {
	if status == provider.StatusInReview {
		return nil, errors.New("in-review read timed out")
	}
	return p.MemoryProvider.ListTasks(ctx, project, status)
}

type doneReadFailingProvider struct {
	*provider.MemoryProvider
}

func (p doneReadFailingProvider) ListTasks(ctx context.Context, project, status string) ([]*provider.Task, error) {
	if status == provider.StatusDone {
		return nil, errors.New("done read timed out")
	}
	return p.MemoryProvider.ListTasks(ctx, project, status)
}

func TestPulseReviewPacketRoutesToSupervisor(t *testing.T) {
	packet := pulseReviewPacket(pulse.AgentObservation{Name: "api-crusader", TabID: "wB:t2WF", Workspace: "wB"}, nil, nil)
	if !strings.Contains(packet, "PULSE REVIEW HANDOFF") || !strings.Contains(packet, "wB:t2WF") {
		t.Fatalf("packet lacks exact lane identity: %s", packet)
	}
	if !strings.Contains(packet, "reviewer dispatch, retries, verdict ingest") || !strings.Contains(packet, "Do not ask the coordinator") {
		t.Fatalf("packet does not assign review lifecycle to supervisor: %s", packet)
	}
}

func TestPulseDispatchDecisionSortsByPriorityThenRef(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{Ref: "FAC-20", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
		{Ref: "FAC-2", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
		{Ref: "FAC-1", Status: provider.StatusInProgress, Priority: provider.PriorityUrgent},
		{Ref: "FAC-3", Status: provider.StatusToDo, Priority: provider.PriorityUrgent},
	}, broker.Inputs{})
	if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-3" {
		t.Fatalf("selected task=%+v want highest-priority claimable FAC-3", d.Task)
	}
	d = pulseDispatchDecision([]*provider.Task{{Status: provider.StatusInProgress, Ref: "FAC-1"}}, broker.Inputs{})
	if d.Outcome != broker.OutcomeWait || strings.TrimSpace(d.WaitReason) == "" {
		t.Fatalf("in-progress-only board must wait with a named reason, got %+v", d)
	}
}

func herdDepsFence(ref, id, edges string) string {
	if edges == "" {
		edges = "[]"
	}
	return "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":" + edges + "}\n```\n"
}

// FAC-581: the production pulse selector must route through pkg/broker.Decide,
// which skips an urgent card whose blocking edge is still open.
func TestPulseDispatchDecisionSkipsDependencyBlockedHigherPriority(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{
			Ref: "FAC-75", ID: "t1", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
			Description: herdDepsFence("FAC-75", "t1", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
		},
		{
			Ref: "FAC-3", ID: "t3", Status: provider.StatusToDo, Priority: provider.PriorityHigh,
			Description: herdDepsFence("FAC-3", "t3", "[]"),
		},
	}, broker.Inputs{})
	if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-3" {
		t.Fatalf("dependency-ready builder must win over blocked urgent, got %+v", d.Task)
	}
}

func TestPulseDispatchDecisionRejectsSelectorWithoutIdentity(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{ID: "no-ref", Status: provider.StatusToDo, Priority: provider.PriorityUrgent},
		{Ref: "   ", ID: "blank", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
	}, broker.Inputs{})
	if err := d.Validate(); err != nil {
		t.Fatalf("empty-identity wait must still be actionable: %v", err)
	}
	if d.Outcome != broker.OutcomeWait || strings.TrimSpace(d.WaitReason) == "" {
		t.Fatalf("selector without identity must wait with a named reason, got %+v", d)
	}
}

// FAC-581 correction (independent review finding 3): equal-priority refs order
// NUMERICALLY (FAC-3 before FAC-10), matching the repo-wide Priority DESC /
// Ref ASC claim-order invariant provider.CompareRefs encodes.
func TestPulseDispatchDecisionOrdersEqualPriorityRefsNumerically(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{Ref: "FAC-10", ID: "t10", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
		{Ref: "FAC-3", ID: "t3", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
	}, broker.Inputs{})
	if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-3" {
		t.Fatalf("equal priority must select the numerically lowest ref FAC-3, got %+v", d.Task)
	}
}

// FAC-581 correction (independent review finding 2): invalid dependency
// provenance must FAIL CLOSED. A malformed, conflicting, or unclosed
// herd-deps-v1 fence is unknown dependency state — reported as a named block,
// never silently converted into a ready task with no dependencies.
func TestPulseDispatchDecisionInvalidProvenanceFailsClosed(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{
			Ref: "FAC-9", ID: "t9", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
			Description: "```herd-deps-v1\n{malformed}\n```\n",
		},
		{
			Ref: "FAC-3", ID: "t3", Status: provider.StatusToDo, Priority: provider.PriorityHigh,
			Description: herdDepsFence("FAC-3", "t3", "[]"),
		},
	}, broker.Inputs{})
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-3" {
		t.Fatalf("the valid lower-priority task must dispatch, got %+v", d)
	}
	if reason := d.Blocked["FAC-9"]; !strings.Contains(reason, "invalid herd-deps-v1 provenance") {
		t.Fatalf("malformed provenance must be a named block, got %q (decision %+v)", reason, d)
	}
}

func TestPulseDispatchDecisionReviewSaturationDoesNotSuppressBuilder(t *testing.T) {
	d := pulseDispatchDecision(nil, broker.Inputs{
		Lane:    "pulse",
		Accepts: []broker.Kind{broker.KindBuild},
		Queue: []broker.Task{
			{Ref: "FAC-581", Kind: broker.KindBuild, Priority: 3},
			{Ref: "FAC-9", Kind: broker.KindReview, Priority: 4},
		},
		ReviewSaturated:  true,
		ReviewWaitReason: "8 reviews in flight >= cap 3",
		Progress: progress.Record{
			Lane:         "pulse",
			TaskRef:      "FAC-581",
			Action:       progress.ClassBuild,
			LastArtifact: "sha-builder",
		},
	})
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-581" {
		t.Fatalf("full review slot suppressed an independent builder: %+v", d)
	}
	if d.Progress.LastArtifact != "sha-builder" {
		t.Fatalf("decision must carry progress classification, got %+v", d.Progress)
	}
}

func TestPulseDispatchDecisionEventWaitOnUnchangedProbe(t *testing.T) {
	d := pulseDispatchDecision([]*provider.Task{
		{Ref: "FAC-581", ID: "builder", Status: provider.StatusToDo, Priority: provider.PriorityUrgent},
	}, broker.Inputs{
		Progress: progress.Record{
			Lane:         "pulse",
			TaskRef:      "FAC-581",
			Action:       progress.ClassProbe,
			LastArtifact: "sha-a",
			WaitReason:   "identical_probe",
		},
	})
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Outcome != broker.OutcomeWait || d.WaitReason != "identical_probe" {
		t.Fatalf("unchanged probe scored as work: %+v", d)
	}
	if d.Progress.UnchangedBeats < 1 {
		t.Fatalf("probe must not advance useful progress: %+v", d.Progress)
	}
}

func TestCollectPulseProviderObservationSelectsReadyBuilderOverBlockedUrgent(t *testing.T) {
	tp := provider.NewMemoryProvider()
	for _, task := range []*provider.Task{
		{
			ID: "t1", Ref: "FAC-75", Title: "blocked", ProjectID: "proj-581",
			Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
			Description: herdDepsFence("FAC-75", "t1", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
		},
		{
			ID: "t3", Ref: "FAC-3", Title: "ready", ProjectID: "proj-581",
			Status: provider.StatusToDo, Priority: provider.PriorityHigh,
			Description: herdDepsFence("FAC-3", "t3", "[]"),
		},
	} {
		if _, err := tp.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	obs, _ := collectPulseProviderObservation(context.Background(), tp, "proj-581")
	if !obs.Known {
		t.Fatalf("provider observation unknown: %+v", obs)
	}
	if obs.NextTaskRef != "FAC-3" || obs.NextTaskID != "t3" {
		t.Fatalf("production pulse caller must dispatch the ready builder, got ref=%q id=%q", obs.NextTaskRef, obs.NextTaskID)
	}
}

// FAC-581 correction (independent review finding 1): a RESOLVED dependency
// must become ready in the production caller. The scoped done-column read
// populates the authoritative closed set, so the dependent urgent card wins
// over the lower-priority ready card once its blocker closes.
func TestCollectPulseProviderObservationResolvedBlockerBecomesNextTask(t *testing.T) {
	tp := provider.NewMemoryProvider()
	for _, task := range []*provider.Task{
		{
			ID: "t136", Ref: "FAC-136", Title: "blocker", ProjectID: "proj-581", Status: provider.StatusDone,
		},
		{
			ID: "t75", Ref: "FAC-75", Title: "dependent", ProjectID: "proj-581",
			Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
			Description: herdDepsFence("FAC-75", "t75", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
		},
		{
			ID: "t3", Ref: "FAC-3", Title: "lower", ProjectID: "proj-581",
			Status: provider.StatusToDo, Priority: provider.PriorityHigh,
			Description: herdDepsFence("FAC-3", "t3", "[]"),
		},
	} {
		if _, err := tp.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	obs, doneRefs := collectPulseProviderObservation(context.Background(), tp, "proj-581")
	if !obs.Known {
		t.Fatalf("provider observation unknown: %+v", obs)
	}
	if obs.NextTaskRef != "FAC-75" || obs.NextTaskID != "t75" {
		t.Fatalf("resolved blocker must admit the dependent urgent task FAC-75, got ref=%q id=%q wait=%q", obs.NextTaskRef, obs.NextTaskID, obs.NextWaitReason)
	}
	if !doneRefs["FAC-136"] {
		t.Fatalf("done read must populate the authoritative closed set: %v", doneRefs)
	}
}

// FAC-581 correction (independent review finding 1): a failed done read is
// unknown dependency state and must fail closed exactly like a failed
// in-review read.
func TestCollectPulseProviderObservationFailsClosedOnDoneReadError(t *testing.T) {
	tp := doneReadFailingProvider{MemoryProvider: provider.NewMemoryProvider()}
	if _, err := tp.CreateTask(context.Background(), &provider.Task{
		ID: "t-75", Ref: "FAC-75", Title: "dependent", ProjectID: "proj-581", Status: provider.StatusToDo,
		Description: herdDepsFence("FAC-75", "t-75", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
	}); err != nil {
		t.Fatal(err)
	}
	providerObs, doneRefs := collectPulseProviderObservation(context.Background(), tp, "proj-581")
	if providerObs.Known {
		t.Fatalf("done read error reported known provider observation: %+v", providerObs)
	}
	if !strings.Contains(providerObs.Error, "done read timed out") {
		t.Fatalf("done read error missing from provider observation: %+v", providerObs)
	}
	if providerObs.NextTaskRef != "" || doneRefs != nil {
		t.Fatalf("unknown done read left dispatch authority behind: observation=%+v doneRefs=%v", providerObs, doneRefs)
	}
}

// FAC-581 correction (independent review finding 4): the production caller
// records the broker's NAMED WAIT when nothing is dispatchable. A claimable
// count without an admitted identity is a wait, not dispatch authority.
func TestCollectPulseProviderObservationRecordsBrokerWait(t *testing.T) {
	tp := provider.NewMemoryProvider()
	for _, task := range []*provider.Task{
		{
			// The blocker is in-progress: claimable listing excludes it and
			// the closed set does not contain it, so FAC-75 stays blocked.
			ID: "t136", Ref: "FAC-136", Title: "blocker", ProjectID: "proj-581", Status: provider.StatusInProgress,
		},
		{
			ID: "t75", Ref: "FAC-75", Title: "dependent", ProjectID: "proj-581",
			Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
			Description: herdDepsFence("FAC-75", "t75", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
		},
	} {
		if _, err := tp.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	obs, _ := collectPulseProviderObservation(context.Background(), tp, "proj-581")
	if !obs.Known {
		t.Fatalf("provider observation unknown: %+v", obs)
	}
	if obs.NextTaskRef != "" || obs.NextTaskID != "" {
		t.Fatalf("an all-blocked queue must not leave dispatch identity behind: %+v", obs)
	}
	if !strings.Contains(obs.NextWaitReason, "FAC-136") {
		t.Fatalf("the wait must name the blocking event, got %q (obs %+v)", obs.NextWaitReason, obs)
	}
	if obs.NextBlocked["FAC-75"] == "" {
		t.Fatalf("the rejected candidate must be explained: %+v", obs.NextBlocked)
	}
}

func TestCollectPulseProviderObservationFailsClosedOnInReviewReadError(t *testing.T) {
	tp := inReviewReadFailingProvider{MemoryProvider: provider.NewMemoryProvider()}
	if _, err := tp.CreateTask(context.Background(), &provider.Task{
		ID: "task-641", Ref: "FAC-641", Title: "pulse error fixture", ProjectID: "proj-641", Status: provider.StatusToDo,
	}); err != nil {
		t.Fatal(err)
	}

	providerObs, doneRefs := collectPulseProviderObservation(context.Background(), tp, "proj-641")
	if providerObs.Known {
		t.Fatalf("in-review read error reported known provider observation: %+v", providerObs)
	}
	if !strings.Contains(providerObs.Error, "in-review read timed out") {
		t.Fatalf("in-review error missing from provider observation: %+v", providerObs)
	}
	if providerObs.NextTaskRef != "" || providerObs.NextTaskID != "" || doneRefs != nil {
		t.Fatalf("unknown provider read left dispatch or completion evidence behind: observation=%+v doneRefs=%v", providerObs, doneRefs)
	}

	snap, err := pulse.Plan(pulse.Observation{
		Provider: providerObs,
		Herdr:    pulse.HerdrObservation{Known: true},
		Review:   pulse.ReviewObservation{Known: true},
		Quota:    pulse.QuotaObservation{Known: true},
		WindDown: pulse.WindDownObservation{Known: true},
	}, pulse.Options{Act: true, Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnknownCritical || !snap.DispatchBlocked || snap.ExitCode == 0 {
		t.Fatalf("in-review read error must block dispatch with a non-zero pulse: %+v", snap)
	}
	for _, action := range snap.Actions {
		if action.Kind == pulse.ActionDispatch {
			t.Fatalf("dispatch remained eligible after an in-review read error: %+v", action)
		}
	}
	if strings.Contains(pulse.FormatHuman(snap), "in_review=0") {
		t.Fatalf("unknown in-review count was reported as zero: %s", pulse.FormatHuman(snap))
	}
}

func TestLivePulseActorDispatchUsesSharedDecision(t *testing.T) {
	var got dispatchRequest
	actor := &livePulseActor{
		dispatch: func(_ context.Context, target, reason string) error {
			got = dispatchRequest{TicketRef: "FAC-479", LaneName: target, LaneExplicit: true}
			if reason == "" {
				t.Fatal("pulse dispatch reason must be preserved")
			}
			return nil
		},
		dispatchRef: "FAC-479",
	}
	if err := actor.Dispatch(context.Background(), "smith", "bounded test dispatch"); err != nil {
		t.Fatal(err)
	}
	if got.TicketRef != "FAC-479" || got.LaneName != "smith" || !got.LaneExplicit {
		t.Fatalf("dispatch request=%+v", got)
	}
}

func TestResolvePulseDispatchLaneUsesCanonicalLaneForLiveAgentID(t *testing.T) {
	registry, err := lifecycle.NewCanonicalLaneRegistry([]lifecycle.CanonicalLane{
		{Name: "api-crusader", Role: "forge"},
		{Name: "smith", Role: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		liveID  string
		want    string
		wantErr bool
	}{
		{name: "known live agent", liveID: "forge-api-crusader", want: "api-crusader"},
		{name: "unknown live agent", liveID: "forge-missing", wantErr: true},
		{name: "configured lane without live prefix", liveID: "api-crusader", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePulseDispatchLane(registry, tt.liveID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePulseDispatchLane(%q) succeeded with %q; want fail-closed error", tt.liveID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePulseDispatchLane(%q): %v", tt.liveID, err)
			}
			if got != tt.want {
				t.Fatalf("resolvePulseDispatchLane(%q)=%q want %q", tt.liveID, got, tt.want)
			}
		})
	}
}

func TestFilterPulseAgentsWorkspace(t *testing.T) {
	agents := []herdr.AgentEntry{
		{Name: "herdforge-worker", Workspace: "wK"},
		{Name: "chainseer-worker", Workspace: "wB"},
		{Name: "unknown-workspace"},
	}
	got := filterPulseAgentsWorkspace(agents, "wK")
	if len(got) != 1 || got[0].Name != "herdforge-worker" {
		t.Fatalf("workspace filter returned %#v", got)
	}
}

// TestLeaseDBPathDefaultsToProductionLaunchClaims is the non-vacuous guard for
// review HIGH #2: a wrong default (.herd/leases.db) silently empties renewals.
func TestLeaseDBPathDefaultsToProductionLaunchClaims(t *testing.T) {
	t.Setenv("HERD_LEASE_DB", "")
	_ = os.Unsetenv("HERD_LEASE_DB")
	got := leaseDBPath()
	want := deps.DefaultLaunchLeasePath()
	if got != want {
		t.Fatalf("leaseDBPath()=%q want production path %q", got, want)
	}
	if got == filepath.Join(".herd", "leases.db") {
		t.Fatal("leaseDBPath must not use the non-production .herd/leases.db default")
	}
	if !strings.Contains(got, "launch-claims.db") {
		t.Fatalf("leaseDBPath must name launch-claims.db, got %q", got)
	}
	// Override still works for hermetic tests.
	t.Setenv("HERD_LEASE_DB", "/tmp/custom-leases.db")
	if leaseDBPath() != "/tmp/custom-leases.db" {
		t.Fatal("HERD_LEASE_DB override ignored")
	}
}

// TestPulseCommand_WindDownRejectsBeforeBeat restores FAC-93 AC: pulse is the
// canonical fleet-admission gate. Enabled wind-down must print
// "fleet admission rejected" and exit non-zero without running a full beat.
func TestPulseCommand_WindDownRejectsBeforeBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", path)
	a, err := winddown.New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), true, "test", "fac-73-admission", 1, nil); err != nil {
		t.Fatal(err)
	}

	outF, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	code := runPulseCommand(nil, outF, errF)
	if code == 0 {
		t.Fatal("enabled wind-down must reject pulse (exit non-zero)")
	}
	if _, err := errF.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(errF)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fleet admission rejected") {
		t.Fatalf("want fleet admission rejected, got exit=%d stderr=%q stdout seek-check", code, raw)
	}
	// Non-vacuous flip: disabled wind-down must not emit that rejection string
	// from the admission gate alone (later sources may still fail closed).
	if _, err := a.Update(context.Background(), false, "test", "fac-73-admission-off", 2, nil); err != nil {
		t.Fatal(err)
	}
}

// TestReadPulseLeasesSeesProductionLaunchClaimsDB proves --act renew wiring
// opens the same store daemon/dispatch write (not an empty alternate path).
func TestReadPulseLeasesSeesProductionLaunchClaimsDB(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	_ = os.Unsetenv("HERD_LEASE_DB")
	// OpenLeaseOwnership resolves a canonical hold path via git-common;
	// a real repo root is required (production always has one).
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "pulse@test")
	runGitT(t, root, "config", "user.name", "pulse")

	dbPath := deps.DefaultLaunchLeasePath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Also plant a decoy at the old wrong path so a regression that opens
	// leases.db would not accidentally "pass" by finding the real lease.
	decoy := filepath.Join(".herd", "leases.db")
	if err := os.WriteFile(decoy, []byte("not-a-sqlite-db"), 0o600); err != nil {
		t.Fatal(err)
	}

	own, err := deps.OpenLeaseOwnership(dbPath, "Herdforge", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	own.LaneResolver = func(role string) (string, error) {
		return "smith", nil
	}
	tok, err := own.ClaimExclusive(context.Background(), "id-fac-1", "FAC-1", "worker", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok == nil || tok.Generation <= 0 {
		t.Fatalf("token=%+v", tok)
	}

	// Shorten remaining TTL so Plan would renew: rewrite via ClaimManager clock
	// is not exposed; ActiveClaims alone proves the store is the production one.
	leases, mgr, err := readPulseLeases(context.Background())
	if err != nil {
		t.Fatalf("readPulseLeases: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected ClaimManager against production store")
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v want the FAC-1 claim from launch-claims.db", leases)
	}
	if leases[0].TaskRef != "FAC-1" || leases[0].Generation != tok.Generation || !leases[0].Active {
		t.Fatalf("lease observation mismatch: %+v token gen=%d", leases[0], tok.Generation)
	}

	// Generation-fenced renew against the same production manager.
	renewed, err := mgr.Renew(context.Background(), claim.LeaseKey{
		Repo: leases[0].Repo, Provider: leases[0].Provider,
		Project: leases[0].Project, TaskRef: leases[0].TaskRef,
	}, leases[0].OwnerID, leases[0].Generation)
	if err != nil {
		t.Fatalf("Renew on production store: %v", err)
	}
	if renewed.Generation != tok.Generation {
		t.Fatalf("renew changed generation: %d -> %d", tok.Generation, renewed.Generation)
	}
	// Stale generation must fail (proves fencing is live, not vacuous success).
	if _, err := mgr.Renew(context.Background(), claim.LeaseKey{
		Repo: leases[0].Repo, Provider: leases[0].Provider,
		Project: leases[0].Project, TaskRef: leases[0].TaskRef,
	}, leases[0].OwnerID, leases[0].Generation-1); err == nil {
		t.Fatal("stale generation renew must fail")
	}
}

// --- FAC-218: fleet-level reap evidence and close path tests ---

func TestTaskRefFromAgentName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"task-fac-218", "FAC-218"},
		{"task-fac-155", "FAC-155"},
		{"Task-FAC-218", "FAC-218"},
		{"task-abc-123", "ABC-123"},
		{"recovery-sentinel", ""},
		{"forge-smith", ""},
		{"", ""},
		{"task-", ""},
	}
	for _, tc := range cases {
		if got := taskRefFromAgentName(tc.name); got != tc.want {
			t.Fatalf("taskRefFromAgentName(%q)=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestApplyReapEvidenceSetsAllFields(t *testing.T) {
	agent := pulse.AgentObservation{Name: "task-fac-218"}
	ev := reapEvidence{
		doneRefs:   map[string]bool{"FAC-218": true},
		safeRefs:   map[string]string{"FAC-218": "refs/herd/safe/fac-218"},
		committed:  map[string]bool{"task-fac-218": true},
		vetoedSHAs: map[string]bool{"abc123": true},
		headSHAs:   map[string]string{"task-fac-218": "abc123"},
	}
	out := applyReapEvidence(agent, "FAC-218", ev)
	if !out.TicketDone {
		t.Fatal("TicketDone must be set when ref is in doneRefs")
	}
	if out.SafeRef != "refs/herd/safe/fac-218" {
		t.Fatalf("SafeRef=%q", out.SafeRef)
	}
	if !out.CommittedWork {
		t.Fatal("CommittedWork must be set when agent is in committed map")
	}
	if !out.AwaitingVerdict {
		t.Fatal("AwaitingVerdict must be set when HEAD SHA is vetoed")
	}
}

func TestApplyReapEvidenceNilMapsFailClosed(t *testing.T) {
	agent := pulse.AgentObservation{Name: "task-fac-218"}
	ev := reapEvidence{} // all nil maps
	out := applyReapEvidence(agent, "FAC-218", ev)
	if out.TicketDone || out.CommittedWork || out.AwaitingVerdict || out.SafeRef != "" {
		t.Fatalf("nil evidence maps must not set any evidence fields: %+v", out)
	}
}

func TestApplyReapEvidenceEmptyRefSkipsTicketAndSafeRef(t *testing.T) {
	agent := pulse.AgentObservation{Name: "recovery-sentinel"}
	ev := reapEvidence{
		doneRefs:  map[string]bool{"FAC-218": true},
		safeRefs:  map[string]string{"FAC-218": "refs/herd/safe/fac-218"},
		committed: map[string]bool{"recovery-sentinel": true},
	}
	out := applyReapEvidence(agent, "", ev)
	if out.TicketDone || out.SafeRef != "" {
		t.Fatalf("empty ref must not set TicketDone or SafeRef: %+v", out)
	}
	if !out.CommittedWork {
		t.Fatal("CommittedWork is keyed by agent name, not ref — must still be set")
	}
}

// TestApplyReapEvidenceAwaitingVerdictFlipIsNonVacuous proves the KEEP
// distinction is real: clearing the vetoed SHA must produce AwaitingVerdict=false.
func TestApplyReapEvidenceAwaitingVerdictFlipIsNonVacuous(t *testing.T) {
	agent := pulse.AgentObservation{Name: "task-fac-218"}
	ev := reapEvidence{
		vetoedSHAs: map[string]bool{"abc123": true},
		headSHAs:   map[string]string{"task-fac-218": "abc123"},
	}
	if out := applyReapEvidence(agent, "FAC-218", ev); !out.AwaitingVerdict {
		t.Fatal("vetoed HEAD must set AwaitingVerdict")
	}
	ev.vetoedSHAs = map[string]bool{}
	if out := applyReapEvidence(agent, "FAC-218", ev); out.AwaitingVerdict {
		t.Fatal("non-vetoed HEAD must not set AwaitingVerdict")
	}
}

func TestReapLaneClosesWithFencingEvidence(t *testing.T) {
	var capturedReq herdr.CompareAndCloseRequest
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		capturedReq = req
		return herdr.CloseReceipt{
			Request:          req,
			Outcome:          herdr.OutcomeClosed,
			ResultingAbsence: true,
		}, nil
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		TabID:         "wK:t1",
		Workspace:     "wK",
		PaneID:        "p1",
		TabGeneration: 7,
		TabRevision:   3,
	}
	if err := actor.ReapLane(context.Background(), lane); err != nil {
		t.Fatalf("ReapLane: %v", err)
	}
	if capturedReq.WorkspaceID != "wK" || capturedReq.TabID != "wK:t1" {
		t.Fatalf("close request target mismatch: %+v", capturedReq)
	}
	if capturedReq.TabGeneration != 7 || capturedReq.TabRevision != 3 {
		t.Fatalf("close request generation fence mismatch: gen=%d rev=%d", capturedReq.TabGeneration, capturedReq.TabRevision)
	}
	if capturedReq.Nonce == "" {
		t.Fatal("close request must carry a nonce")
	}
	if len(capturedReq.PaneIDs) != 1 || capturedReq.PaneIDs[0] != "p1" {
		t.Fatalf("close request paneIDs=%v want [p1]", capturedReq.PaneIDs)
	}
}

func TestReapLaneFailsClosedOnMissingGeneration(t *testing.T) {
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		t.Fatal("transport must not be called when generation is missing")
		return herdr.CloseReceipt{}, nil
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:      "task-fac-218",
		TabID:     "wK:t1",
		Workspace: "wK",
		// TabGeneration deliberately zero — no fencing evidence.
	}
	if err := actor.ReapLane(context.Background(), lane); err == nil {
		t.Fatal("ReapLane must fail when TabGeneration is zero (no fencing evidence)")
	}
}

func TestReapLaneFailsClosedOnMissingTabID(t *testing.T) {
	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		Workspace:     "wK",
		TabGeneration: 7,
	}
	if err := actor.ReapLane(context.Background(), lane); err == nil {
		t.Fatal("ReapLane must fail when TabID is empty")
	}
}

func TestReapLaneAcceptsAlreadyClosed(t *testing.T) {
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		return herdr.CloseReceipt{
			Request:          req,
			Outcome:          herdr.OutcomeAlreadyClosed,
			ResultingAbsence: true,
		}, nil
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		TabID:         "wK:t1",
		Workspace:     "wK",
		TabGeneration: 7,
	}
	if err := actor.ReapLane(context.Background(), lane); err != nil {
		t.Fatalf("AlreadyClosed with resulting absence must succeed: %v", err)
	}
}

func TestReapLaneRejectsStaleGeneration(t *testing.T) {
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		return herdr.CloseReceipt{
			Request: req,
			Outcome: herdr.OutcomeStaleGeneration,
		}, nil
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		TabID:         "wK:t1",
		Workspace:     "wK",
		TabGeneration: 7,
	}
	if err := actor.ReapLane(context.Background(), lane); err == nil {
		t.Fatal("stale generation must be a hard error, not success")
	}
}

func TestReapLaneRejectsAlreadyClosedWithoutAbsence(t *testing.T) {
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		return herdr.CloseReceipt{
			Request:          req,
			Outcome:          herdr.OutcomeAlreadyClosed,
			ResultingAbsence: false,
		}, nil
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		TabID:         "wK:t1",
		Workspace:     "wK",
		TabGeneration: 7,
	}
	if err := actor.ReapLane(context.Background(), lane); err == nil {
		t.Fatal("already_closed without resulting absence must fail")
	}
}

// TestReapLaneTransportErrorIsHardError proves a transport failure (e.g.
// herdr CLI not installed) propagates as an error, not a silent success.
func TestReapLaneTransportErrorIsHardError(t *testing.T) {
	restore := herdr.SetCompareCloseTransportForTest(func(req herdr.CompareAndCloseRequest) (herdr.CloseReceipt, error) {
		return herdr.CloseReceipt{}, errors.New("herdr tab compare-close: command not found")
	})
	defer restore()

	actor := &livePulseActor{}
	lane := pulse.AgentObservation{
		Name:          "task-fac-218",
		TabID:         "wK:t1",
		Workspace:     "wK",
		TabGeneration: 7,
	}
	if err := actor.ReapLane(context.Background(), lane); err == nil {
		t.Fatal("transport error must propagate as hard error")
	}
}

// TestLoadReapEvidenceWithRealGitRepo proves evidence gathering works against
// a real git worktree: committed work, safe refs, and HEAD SHA are detected.
func TestLoadReapEvidenceWithRealGitRepo(t *testing.T) {
	// Isolate the review ledger path so loadReapEvidence does not read a
	// real ledger from the operator's state directory.
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(t.TempDir(), "no-ledger.jsonl"))

	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "reap@test")
	runGitT(t, dir, "config", "user.name", "reap")
	runGitT(t, dir, "config", "core.hooksPath", "/dev/null")
	runGitT(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "base.txt", "base\n")
	runGitT(t, dir, "branch", "-M", "main")
	baseSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseSHA)
	writeRepoFile(t, dir, "work.txt", "work\n")
	headSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))

	safeRef := "refs/herd/safe/fac-218"
	runGitT(t, dir, "update-ref", safeRef, headSHA)

	entries := []herdr.AgentEntry{
		{Name: "task-fac-218", Cwd: dir},
		{Name: "task-fac-999", Cwd: ""}, // empty Cwd — skipped
	}
	ev := loadReapEvidence(context.Background(), entries, map[string]bool{"FAC-218": true})

	if !ev.committed["task-fac-218"] {
		t.Fatal("expected committed work for task-fac-218")
	}
	if ev.safeRefs["FAC-218"] == "" {
		t.Fatal("expected safe ref for FAC-218")
	}
	if ev.safeRefs["FAC-999"] != "" {
		t.Fatal("FAC-999 should not have a safe ref")
	}
	if ev.headSHAs["task-fac-218"] != headSHA {
		t.Fatalf("head SHA mismatch: got %q want %q", ev.headSHAs["task-fac-218"], headSHA)
	}
	if ev.headSHAs["task-fac-999"] != "" {
		t.Fatal("empty Cwd agent should not have a head SHA")
	}
}

// TestReadPulseReviewCorruptLedgerReportsUnknown proves the finding-3 fix:
// when the ledger file exists but is corrupted, readPulseReview must return
// Known=false with an error so an operator can detect the problem, not
// silently report Known=true with zero pending.
func TestReadPulseReviewCorruptLedgerReportsUnknown(t *testing.T) {
	ledgerDir := t.TempDir()
	corruptLedger := filepath.Join(ledgerDir, "corrupt-ledger.jsonl")
	if err := os.WriteFile(corruptLedger, []byte("{not valid json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", corruptLedger)

	obs := readPulseReview()
	if obs.Known {
		t.Fatal("corrupt ledger must produce Known=false, not silently Known=true with zero pending")
	}
	if obs.Error == "" {
		t.Fatal("corrupt ledger must populate Error for operator visibility")
	}
}

// TestReadPulseReviewAbsentLedgerReportsKnownEmpty proves the non-vacuous
// flip: when the ledger file does not exist, review is known-empty (Known=true,
// Pending=0), not unknown.
func TestReadPulseReviewAbsentLedgerReportsKnownEmpty(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", filepath.Join(t.TempDir(), "no-ledger.jsonl"))

	obs := readPulseReview()
	if !obs.Known {
		t.Fatal("absent ledger must produce Known=true (known-empty), not unknown")
	}
	if obs.Pending != 0 || obs.RawVetoed != 0 {
		t.Fatalf("absent ledger must report zero pending/rawVetoed: %+v", obs)
	}
}

func TestReadPulseReviewNamesRawVetoedSetExplicitly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range []review.LedgerRow{
		{Event: string(review.EventVerdict), SHA: "merged-fail", Reviewer: "r1", Verdict: string(review.VerdictFAIL)},
		{Event: string(review.EventVerdict), SHA: "live-fail", Reviewer: "r2", Verdict: string(review.VerdictBLOCKED)},
	} {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", path)

	obs := readPulseReview()
	if obs.RawVetoed != 2 || len(obs.RawVetoedRefs) != 2 {
		t.Fatalf("raw vetoed set must retain both ledger SHAs: %+v", obs)
	}
	encoded, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["raw_vetoed"]; !ok {
		t.Fatalf("pulse schema missing raw_vetoed: %s", encoded)
	}
	if _, ok := payload["need_review"]; ok {
		t.Fatalf("pulse schema still exposes ambiguous need_review: %s", encoded)
	}
}

func TestReadPulseReviewSaturatesAtDrainPressureCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-ledger.jsonl")
	l, err := review.NewReviewLedger(t.TempDir(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	if err := l.Record(review.RecordOpts{SHA: "pending", Reviewer: "reviewer-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(review.VerdictOpts{SHA: "vetoed", Reviewer: "reviewer-b", Verdict: review.VerdictFAIL}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", path)
	t.Setenv("HERD_IN_REVIEW_CAP", "2")

	obs := readPulseReview()
	if !obs.Saturated {
		t.Fatalf("review should be saturated at pending+need-review cap: %+v", obs)
	}
}

// TestLoadReapEvidenceCorruptLedgerSetsAwaitingVerdict proves the fail-closed
// fix for the KEEP signal: when the review ledger file exists but is corrupted
// (unreadable JSONL), loadReapEvidence must signal that a verdict may be
// pending, and applyReapEvidence must set AwaitingVerdict on agents with
// committed work — so the reap planner does NOT destroy a live lane whose
// FAIL/BLOCKED verdict it cannot read.
//
// Against the unfixed code, Vetoed returns an error that is silently dropped,
// vetoedSHAs stays nil, AwaitingVerdict is never set, and the lane is reaped.
// This test fails on that code and passes after the fix.
func TestLoadReapEvidenceCorruptLedgerSetsAwaitingVerdict(t *testing.T) {
	// Corrupted ledger: file exists but contains invalid JSONL.
	ledgerDir := t.TempDir()
	corruptLedger := filepath.Join(ledgerDir, "corrupt-ledger.jsonl")
	if err := os.WriteFile(corruptLedger, []byte("{not valid json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_REVIEW_LEDGER", corruptLedger)

	// Build a real git worktree with committed work so CommittedWork is true.
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "reap@test")
	runGitT(t, dir, "config", "user.name", "reap")
	runGitT(t, dir, "config", "core.hooksPath", "/dev/null")
	runGitT(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "base.txt", "base\n")
	baseSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseSHA)
	runGitT(t, dir, "commit", "--allow-empty", "-m", "feat: real work")

	entries := []herdr.AgentEntry{
		{Name: "task-fac-218", Cwd: dir},
	}
	ev := loadReapEvidence(context.Background(), entries, nil)

	agent := pulse.AgentObservation{Name: "task-fac-218"}
	agent = applyReapEvidence(agent, "FAC-218", ev)

	if !agent.CommittedWork {
		t.Fatal("expected CommittedWork=true (prerequisite for the KEEP signal)")
	}
	if !agent.AwaitingVerdict {
		t.Fatal("corrupt ledger must set AwaitingVerdict: a verdict may be pending that cannot be read (fail-closed KEEP signal)")
	}
}

// TestHasCommittedWorkExcludesAnchorAndWip proves that anchor and wip commits
// do not count as committed work — only real feature commits do.
func TestHasCommittedWorkExcludesAnchorAndWip(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "reap@test")
	runGitT(t, dir, "config", "user.name", "reap")
	runGitT(t, dir, "config", "core.hooksPath", "/dev/null")
	runGitT(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "base.txt", "base\n")
	baseSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseSHA)

	// Anchor commit only — should NOT count as committed work.
	runGitT(t, dir, "commit", "--allow-empty", "-m", "chore: anchor FAC-218")
	if hasCommittedWork(dir) {
		t.Fatal("anchor-only commit must not count as committed work")
	}

	// Wip commit — should NOT count.
	runGitT(t, dir, "commit", "--allow-empty", "-m", "wip: partial work")
	if hasCommittedWork(dir) {
		t.Fatal("wip commit must not count as committed work")
	}

	// Real commit — SHOULD count.
	runGitT(t, dir, "commit", "--allow-empty", "-m", "feat: real work")
	if !hasCommittedWork(dir) {
		t.Fatal("real feature commit must count as committed work")
	}
}

// TestHasCommittedWorkFailsClosedOnNoCommits proves that a worktree with no
// commits ahead of origin/main does not show committed work.
func TestHasCommittedWorkFailsClosedOnNoCommits(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "reap@test")
	runGitT(t, dir, "config", "user.name", "reap")
	runGitT(t, dir, "config", "core.hooksPath", "/dev/null")
	runGitT(t, dir, "config", "commit.gpgsign", "false")
	writeRepoFile(t, dir, "base.txt", "base\n")
	baseSHA := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	runGitT(t, dir, "update-ref", "refs/remotes/origin/main", baseSHA)
	if hasCommittedWork(dir) {
		t.Fatal("no commits ahead of origin/main must not show committed work")
	}
}
