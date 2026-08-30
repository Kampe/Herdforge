package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func holdFixture(t *testing.T) (*HoldAuthority, HoldIdentity, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	a, err := NewHoldAuthorityWithClock(filepath.Join(t.TempDir(), "lifecycle.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, HoldIdentity{Repository: "repo", Owner: "owner", Lane: "lane", Task: "FAC-69", Scope: "task"}, now
}

func TestHoldAuthorityReleaseIsFencedAndExactlyOnce(t *testing.T) {
	a, id, now := holdFixture(t)
	expires := now.Add(time.Hour)
	created, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 1, &expires)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Held || created.Generation != 1 || created.ExpiresAt == nil {
		t.Fatalf("bad hold record: %+v", created)
	}
	if _, err := a.Release(context.Background(), id, "actor", "release", "operator_release", 2); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("future release error=%v", err)
	}
	released, err := a.Release(context.Background(), id, "actor", "release", "operator_release", 1)
	if err != nil {
		t.Fatal(err)
	}
	if released.Held || released.ReleasedAt == nil || released.ExpiresAt == nil {
		t.Fatalf("release lost durable fields: %+v", released)
	}
	replay, err := a.Release(context.Background(), id, "actor", "release", "operator_release", 1)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ReleasedAt == nil || replay.Generation != 1 || replay.Held {
		t.Fatalf("release replay changed state: %+v", replay)
	}
	if _, err := a.Release(context.Background(), id, "other", "different", "operator_release", 1); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("different release payload error=%v", err)
	}
	decision, err := a.Check(context.Background(), id, 1)
	if err != nil || decision.Held {
		t.Fatalf("released check=%+v err=%v", decision, err)
	}
}

func TestHoldAuthorityAbsentIsExplicitlyUnheldAtPositiveGeneration(t *testing.T) {
	a, id, _ := holdFixture(t)
	decision, err := a.Check(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Held || decision.Generation != 1 || decision.Code != "unheld" {
		t.Fatalf("absent decision=%+v", decision)
	}
	if _, err := a.Check(context.Background(), id, 0); !errors.Is(err, ErrHoldAuthorityUnavailable) || errors.Is(err, ErrHoldDenied) {
		t.Fatalf("zero generation error=%v", err)
	}
}

func TestHoldAuthorityRejectsPaddedIdentityBeforeWrite(t *testing.T) {
	a, id, _ := holdFixture(t)
	padded := id
	padded.Owner = " owner"
	if _, err := a.Hold(context.Background(), padded, "actor", "maintenance", "operator_hold", 1, nil); !errors.Is(err, ErrHoldCorrupt) {
		t.Fatalf("padded identity was not rejected: %v", err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_hold_state`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("padded identity wrote hold state: %d rows", count)
	}
}

func TestWithUnheldTransitionReleasedExpiredRowDoesNotRewriteHistory(t *testing.T) {
	a, id, now := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 1, ptrTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Release(context.Background(), id, "actor", "explicit", "operator_release", 1); err != nil {
		t.Fatal(err)
	}
	called := 0
	if err := a.WithUnheldTransition(context.Background(), []HoldIdentity{id}, func() error { called++; return nil }); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("callback count=%d, want 1", called)
	}
	var releases int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_hold_events WHERE repository=? AND owner=? AND lane=? AND task=? AND generation=? AND intent='release'`, id.Repository, id.Owner, id.Lane, id.Task, 1).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release event count=%d, want 1", releases)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestHoldAuthorityFirstGenerationIsExactlyOne(t *testing.T) {
	a, id, _ := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 2, nil); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("first generation error=%v", err)
	}
	if _, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHoldAuthorityActiveGenerationCannotBeOverwritten(t *testing.T) {
	a, id, _ := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Hold(context.Background(), id, "actor", "replacement", "operator_hold", 2, nil); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("active overwrite error=%v", err)
	}
}

func TestHoldAuthorityActorIsProvenanceNotIdentity(t *testing.T) {
	a, id, _ := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor-a", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	replay, err := a.Hold(context.Background(), id, "actor-b", "maintenance", "operator_hold", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Held || replay.Actor != "actor-a" {
		t.Fatalf("actor provenance was not preserved: %+v", replay)
	}
	if _, err := a.Hold(context.Background(), id, "actor-b", "different", "operator_hold", 1, nil); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("payload conflict=%v", err)
	}
}

func TestHoldAuthorityAdvanceRequiresExactlyNextGeneration(t *testing.T) {
	a, id, _ := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor", "one", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Hold(context.Background(), id, "actor", "three", "operator_hold", 3, nil); !errors.Is(err, ErrHoldConflict) {
		t.Fatalf("skipped generation=%v", err)
	}
}

func TestHoldAuthorityExpiryRecordsReleaseOnce(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a, err := NewHoldAuthorityWithClock(filepath.Join(t.TempDir(), "lifecycle.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := HoldIdentity{Repository: "repo", Owner: "owner", Lane: "lane", Task: "FAC-69", Scope: "task"}
	if _, err := a.Hold(context.Background(), id, "actor", "temporary", "operator_hold", 1, func() *time.Time { v := now.Add(time.Minute); return &v }()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	decision, err := a.Check(context.Background(), id, 1)
	if err != nil || decision.Held {
		t.Fatalf("expired check=%+v err=%v", decision, err)
	}
	second, err := a.Check(context.Background(), id, 1)
	if err != nil || second.Held {
		t.Fatalf("expiry replay=%+v err=%v", second, err)
	}
	var events int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_hold_events WHERE repository=? AND owner=? AND lane=? AND task=? AND generation=? AND intent='release'`, id.Repository, id.Owner, id.Lane, id.Task, 1).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("release event count=%d, want 1", events)
	}
}

func TestHoldAuthorityConcurrentHoldReleaseHasOneWinnerPerGeneration(t *testing.T) {
	a, id, now := holdFixture(t)
	if _, err := a.Hold(context.Background(), id, "actor", "initial", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var releaseErr, holdErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, releaseErr = a.Release(context.Background(), id, "actor", "release", "operator_release", 1)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, holdErr = a.Hold(context.Background(), id, "actor", "advance", "operator_hold", 2, nil)
	}()
	close(start)
	wg.Wait()
	if releaseErr != nil && holdErr != nil {
		t.Fatalf("both concurrent transitions failed: release=%v hold=%v", releaseErr, holdErr)
	}
	decision, err := a.Check(context.Background(), id, func() int64 {
		if holdErr == nil {
			return 2
		}
		return 1
	}())
	_ = now
	if err != nil {
		t.Fatal(err)
	}
	if holdErr == nil && !decision.Held {
		t.Fatal("new generation must remain held")
	}
}

type countingHoldReader struct{ calls int }

func (r *countingHoldReader) Check(context.Context, HoldIdentity, int64) (HoldDecision, error) {
	r.calls++
	return HoldDecision{}, nil
}

func TestLifecycleRejectsInvalidActionTupleBeforeAuthorityOrSideEffect(t *testing.T) {
	r := &countingHoldReader{}
	e := &Engine{HoldReader: r, HoldRoles: []string{"forge-smith", "worker"}}
	if err := e.checkHold("FAC-69", "", "owner"); err == nil {
		t.Fatal("invalid lifecycle tuple was admitted")
	}
	if r.calls != 0 {
		t.Fatalf("authority received invalid tuple %d times", r.calls)
	}
}

func TestLifecycleMissingOrUnknownLaneResolverFailsBeforeAuthority(t *testing.T) {
	for _, resolver := range []func(string) (string, error){nil, func(string) (string, error) {
		return "", fmt.Errorf("unknown configured role")
	}} {
		r := &countingHoldReader{}
		e := &Engine{HoldReader: r, HoldLaneResolver: resolver}
		if !e.holdBlocks(context.Background(), "worker", "worker", "FAC-69") {
			t.Fatal("unresolved lane was not occupied fail-closed")
		}
		if r.calls != 0 {
			t.Fatalf("unresolved lane reached authority %d times", r.calls)
		}
	}
}

func TestLifecycleHeldCandidateIsOccupiedBeforeDispatchable(t *testing.T) {
	calls := 0
	identities := []HoldIdentity{}
	e := &Engine{HoldReader: countingHeldSummaryReader{calls: &calls, identities: &identities}, HoldLaneResolver: func(role string) (string, error) {
		if role == "owner" {
			return "lane", nil
		}
		return "", fmt.Errorf("unknown role")
	}}
	if !e.holdBlocks(context.Background(), "lane", "owner", "FAC-69") {
		t.Fatal("held candidate was not occupied before selection")
	}
	if calls == 0 {
		t.Fatal("held candidate did not consult authority")
	}
	if len(identities) != 2 {
		t.Fatalf("authority identities=%+v, want lane then task", identities)
	}
	laneIdentity, taskIdentity := identities[0], identities[1]
	if laneIdentity.Repository == "" || laneIdentity.Owner != "owner" || laneIdentity.Lane != "lane" || laneIdentity.Task != "" || laneIdentity.Scope != "lane" {
		t.Fatalf("malformed lane authority identity=%+v", laneIdentity)
	}
	if taskIdentity.Repository == "" || taskIdentity.Owner != "owner" || taskIdentity.Lane != "lane" || taskIdentity.Task != "FAC-69" || taskIdentity.Scope != "task" {
		t.Fatalf("malformed task authority identity=%+v", taskIdentity)
	}
}

type heldSummaryReader struct{}

type countingHeldSummaryReader struct {
	calls      *int
	identities *[]HoldIdentity
}

func (r countingHeldSummaryReader) Check(_ context.Context, identity HoldIdentity, _ int64) (HoldDecision, error) {
	(*r.calls)++
	*r.identities = append(*r.identities, identity)
	if identity.Scope == "task" {
		return HoldDecision{Held: true, Reason: "maintenance", Code: "operator_hold"}, nil
	}
	if identity.Scope == "lane" {
		return HoldDecision{Held: false}, nil
	}
	return HoldDecision{}, nil
}

func (r countingHeldSummaryReader) CurrentGeneration(context.Context, HoldIdentity) (int64, error) {
	return 1, nil
}

func (heldSummaryReader) Check(context.Context, HoldIdentity, int64) (HoldDecision, error) {
	return HoldDecision{Held: true, Reason: "maintenance", Code: "operator_hold"}, nil
}

type kaneoShapeReader struct {
	identities  []HoldIdentity
	generations []int64
}

func (r *kaneoShapeReader) CurrentGeneration(context.Context, HoldIdentity) (int64, error) {
	return 1, nil
}

func (r *kaneoShapeReader) Check(_ context.Context, id HoldIdentity, generation int64) (HoldDecision, error) {
	r.identities = append(r.identities, id)
	r.generations = append(r.generations, generation)
	if id.Owner == "forge-smith" {
		return HoldDecision{Held: true}, nil
	}
	return HoldDecision{}, nil
}

func TestKaneoShapeRolesFenceOnlyMatchingLaneAndTask(t *testing.T) {
	r := &kaneoShapeReader{}
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{{Name: "smith", Role: "worker"}, {Name: "scout", Role: "forge-smith"}})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{HoldReader: r, HoldRoles: []string{"forge-smith", "worker"}, HoldLaneResolver: func(role string) (string, error) {
		lane, resolveErr := registry.ResolveRole(role)
		if resolveErr != nil {
			return "", resolveErr
		}
		return lane.Name, nil
	}, HoldIdentity: func(task, _, owner string) HoldIdentity {
		lane, resolveErr := registry.ResolveRole(owner)
		if resolveErr != nil {
			return HoldIdentity{}
		}
		return HoldIdentity{Repository: "repo", Owner: lane.Role, Lane: lane.Name, Task: task, Scope: map[bool]string{true: "task", false: "lane"}[task != ""]}
	}}
	agents := struct {
		Result struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"result"`
	}{}
	board := json.RawMessage(`[{"id":"task-1","ref":"FAC-1","status":"to-do","userId":null,"labels":["forge-smith"]},{"id":"task-2","ref":"FAC-2","status":"to-do","userId":null,"labels":["worker"]}]`)
	s := e.computeSummary(agents, mustParseBoardCards(t, board), nil, nil)
	if s.Dispatchable != 1 || len(s.OccupiedRefs) != 1 || s.OccupiedRefs[0] != "FAC-1" {
		t.Fatalf("summary=%+v", s)
	}
	for _, id := range r.identities {
		if id.Owner == "" || id.Lane == "" || (id.Scope == "task" && id.Task == "") {
			t.Fatalf("malformed authority target=%+v", id)
		}
	}
	if len(r.generations) == 0 {
		t.Fatal("authority Check was never called")
	}
	for _, generation := range r.generations {
		if generation != 1 {
			t.Fatalf("authority received generation %d, want 1", generation)
		}
	}
}

func TestRecognizedRoleUsesConfiguredSetAndRejectsUnknownOrAmbiguous(t *testing.T) {
	roles := []string{"reviewer", "recovery-sentinel"}
	if got := recognizedRole([]string{"reviewer"}, roles); got != "reviewer" {
		t.Fatalf("configured role=%q", got)
	}
	if got := recognizedRole([]string{"forge-smith"}, roles); got != "" {
		t.Fatalf("unknown role=%q", got)
	}
	if got := recognizedRole([]string{"reviewer", "recovery-sentinel"}, roles); got != "" {
		t.Fatalf("ambiguous roles=%q", got)
	}
	if got := recognizedRole([]string{"reviewer", "reviewer"}, roles); got != "" {
		t.Fatalf("duplicate role=%q", got)
	}
}

func TestCanonicalLaunchDBPathFailsClosedOutsideGit(t *testing.T) {
	if _, err := CanonicalStatePathForLaunchDB(filepath.Join(t.TempDir(), ".herd", "launch-claims.db")); err == nil {
		t.Fatal("non-git launch DB unexpectedly produced a canonical path")
	}
	if got := CanonicalStatePath(t.TempDir()); !strings.HasSuffix(got, filepath.Join(".herd", "herdforge.db")) {
		t.Fatalf("explicit root path=%q", got)
	}
}

func TestCheckLaneAndTaskHoldZeroOneAndAmbiguous(t *testing.T) {
	reader := holdDecisionReader{}
	zero := func(context.Context, string) ([]HoldIdentity, error) { return nil, nil }
	generation := func(context.Context, HoldIdentity) (int64, error) { return 1, nil }
	if err := CheckLaneAndTaskHold(context.Background(), reader, zero, "repo", "worker", "worker", generation); err != nil {
		t.Fatalf("zero active tasks blocked lane: %v", err)
	}
	one := func(context.Context, string) ([]HoldIdentity, error) {
		return []HoldIdentity{{Repository: "repo", Owner: "worker", Lane: "worker", Task: "FAC-1", Scope: "task"}}, nil
	}
	if err := CheckLaneAndTaskHold(context.Background(), heldTaskReader{}, one, "repo", "worker", "worker", generation); err == nil {
		t.Fatal("task-held lane was admitted")
	}
	many := func(context.Context, string) ([]HoldIdentity, error) {
		return []HoldIdentity{{Repository: "repo", Owner: "worker", Lane: "worker", Task: "FAC-1", Scope: "task"}, {Repository: "repo", Owner: "worker", Lane: "worker", Task: "FAC-2", Scope: "task"}}, nil
	}
	if err := CheckLaneAndTaskHold(context.Background(), reader, many, "repo", "worker", "worker", generation); err == nil {
		t.Fatal("ambiguous active tasks were admitted")
	}
}

func TestTypedCanonicalNamespacesKeepScoutRoleAndSmithLiveIDDistinct(t *testing.T) {
	r, err := NewCanonicalLaneRegistry([]CanonicalLane{{Name: "smith", Role: "worker"}, {Name: "scout", Role: "forge-smith"}})
	if err != nil {
		t.Fatal(err)
	}
	role, err := r.ResolveRole("forge-smith")
	if err != nil || role.Name != "scout" {
		t.Fatalf("role=%+v err=%v", role, err)
	}
	live, err := r.ResolveLiveAgentID("forge-smith")
	if err != nil || live.Name != "smith" || live.Role != "worker" {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	if _, err := r.ResolveLaneName("forge-smith"); err == nil {
		t.Fatal("live ID accepted as lane name")
	}
}

type holdDecisionReader struct{}

func (holdDecisionReader) Check(context.Context, HoldIdentity, int64) (HoldDecision, error) {
	return HoldDecision{}, nil
}

type heldTaskReader struct{}

func (heldTaskReader) Check(_ context.Context, id HoldIdentity, _ int64) (HoldDecision, error) {
	return HoldDecision{Held: id.Scope == "task"}, nil
}

// FAC-702: one message covered three different problems with three different
// owners. Measured live, seven lanes reported "unknown or ambiguous" and the
// operator could not tell which failure any of them was -- docs-custodian had
// 23 in-progress cards (board hygiene), while qa-sentinel had ZERO and was
// failing for an entirely different reason.
func TestResolverFailureIsNotReportedAsAmbiguity(t *testing.T) {
	resolver := func(context.Context, string) ([]HoldIdentity, error) {
		return nil, errors.New("board unreachable")
	}
	err := CheckLaneAndTaskHold(context.Background(), stubReader{}, resolver, "repo", "owner", "qa-sentinel", stubGeneration)
	if err == nil {
		t.Fatal("a failed resolver was admitted")
	}
	if !strings.Contains(err.Error(), "NOT an ambiguous lane") {
		t.Fatalf("resolver failure still reads as ambiguity: %v", err)
	}
	if !strings.Contains(err.Error(), "board unreachable") {
		t.Fatalf("the underlying cause was swallowed: %v", err)
	}
}

func TestAmbiguousLaneNamesTheTasksToMove(t *testing.T) {
	// The remedy is to move cards out of in-progress, which is impossible
	// without knowing which ones.
	resolver := func(context.Context, string) ([]HoldIdentity, error) {
		return []HoldIdentity{
			{Repository: "repo", Owner: "owner", Lane: "docs-custodian", Task: "CHA-2", Scope: "task"},
			{Repository: "repo", Owner: "owner", Lane: "docs-custodian", Task: "CHA-1", Scope: "task"},
		}, nil
	}
	err := CheckLaneAndTaskHold(context.Background(), stubReader{}, resolver, "repo", "owner", "docs-custodian", stubGeneration)
	if err == nil {
		t.Fatal("an ambiguous lane was admitted")
	}
	for _, want := range []string{"has 2 active tasks", "CHA-1", "CHA-2", "move all but one"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

func TestZeroActiveTasksIsACleanPass(t *testing.T) {
	// Zero is not ambiguous. A lane with no active task binding is simply
	// unbound, and treating that as a failure would fence every idle lane.
	resolver := func(context.Context, string) ([]HoldIdentity, error) { return nil, nil }
	if err := CheckLaneAndTaskHold(context.Background(), stubReader{}, resolver, "repo", "owner", "qa-sentinel", stubGeneration); err != nil {
		t.Fatalf("a lane with no active task was refused: %v", err)
	}
}

type stubReader struct{}

func (stubReader) Check(context.Context, HoldIdentity, int64) (HoldDecision, error) {
	return HoldDecision{}, nil
}

func stubGeneration(context.Context, HoldIdentity) (int64, error) { return 1, nil }

// FAC-593: the single-candidate mismatch branch named the lane and nothing
// else. Live it read "active task binding is unknown or ambiguous: lane=api"
// with a card sitting right there in the resolver result -- so the operator
// could not tell whether the lane matched nothing, matched two, or matched one
// card that failed a field check, nor which field. A refusal that hides the
// candidate it rejected cannot be acted on.
func TestSingleCandidateMismatchNamesTheCardAndTheDiscriminator(t *testing.T) {
	resolver := func(context.Context, string) ([]HoldIdentity, error) {
		return []HoldIdentity{
			{Repository: "repo", Owner: "owner", Lane: "other-lane", Task: "CHA-1784", Scope: "task"},
		}, nil
	}
	err := CheckLaneAndTaskHold(context.Background(), stubReader{}, resolver, "repo", "owner", "api-crusader", stubGeneration)
	if err == nil {
		t.Fatal("a mismatched candidate was admitted")
	}
	for _, want := range []string{"lane=api-crusader", "CHA-1784", "lane", "other-lane", "rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// A candidate with no ref at all must still be distinguishable from a lane that
// matched nothing: the remedy differs (fix the card identity vs. bind a card).
func TestSingleCandidateWithNoRefIsNotReportedAsNoCandidate(t *testing.T) {
	resolver := func(context.Context, string) ([]HoldIdentity, error) {
		return []HoldIdentity{{Repository: "repo", Owner: "owner", Lane: "api-crusader", Scope: "task"}}, nil
	}
	err := CheckLaneAndTaskHold(context.Background(), stubReader{}, resolver, "repo", "owner", "api-crusader", stubGeneration)
	if err == nil {
		t.Fatal("a candidate with no ref was admitted")
	}
	if !strings.Contains(err.Error(), "1 candidate") {
		t.Fatalf("a rejected candidate reads as no candidate: %v", err)
	}
}
