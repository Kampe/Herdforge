package boardproj

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// ---------- doubles ----------

// fakeBoard implements provider.TaskProvider. driftTo, when set, makes
// GetTask report a status other than the one UpdateStatus stored — the exact
// "provider accepted the write, the board says something else" case.
type fakeBoard struct {
	mu       sync.Mutex
	tasks    map[string]*provider.Task
	comments map[string][]string
	driftTo  string
	writes   int
	failNext error
}

func newBoard(taskID, status string) *fakeBoard {
	return &fakeBoard{
		tasks:    map[string]*provider.Task{taskID: {ID: taskID, Ref: "FAC-143", Status: status}},
		comments: map[string][]string{},
	}
}

func (b *fakeBoard) GetTask(_ context.Context, id string) (*provider.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok {
		return nil, fmt.Errorf("no task %s", id)
	}
	cp := *t
	if b.driftTo != "" {
		cp.Status = b.driftTo
	}
	return &cp, nil
}

func (b *fakeBoard) ListTasks(context.Context, string, string) ([]*provider.Task, error) {
	return nil, nil
}
func (b *fakeBoard) ClaimTask(context.Context, string, string) error { return nil }

func (b *fakeBoard) UpdateStatus(_ context.Context, id, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failNext != nil {
		err := b.failNext
		b.failNext = nil
		return err
	}
	t, ok := b.tasks[id]
	if !ok {
		return fmt.Errorf("no task %s", id)
	}
	t.Status = provider.NormalizeStatus(status)
	b.writes++
	return nil
}

func (b *fakeBoard) AddComment(_ context.Context, id, body string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.comments[id] = append(b.comments[id], body)
	return nil
}

func (b *fakeBoard) status(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tasks[id].Status
}

type fakeLabels struct {
	mu     sync.Mutex
	rows   map[string]provider.TaskLabel // label id -> row
	nextID int
}

func newLabels() *fakeLabels { return &fakeLabels{rows: map[string]provider.TaskLabel{}} }

func (l *fakeLabels) ListTaskLabels(_ context.Context, taskID string) ([]provider.TaskLabel, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []provider.TaskLabel
	for _, r := range l.rows {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (l *fakeLabels) CreateTaskLabel(_ context.Context, taskID, name string) (provider.TaskLabel, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	row := provider.TaskLabel{ID: fmt.Sprintf("lbl-%d", l.nextID), Name: name}
	l.rows[row.ID] = row
	return row, nil
}

func (l *fakeLabels) AttachTaskLabel(_ context.Context, taskID, labelID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[labelID]
	if !ok {
		return fmt.Errorf("no label %s", labelID)
	}
	row.TaskID = taskID
	l.rows[labelID] = row
	return nil
}

func (l *fakeLabels) DetachTaskLabel(_ context.Context, labelID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[labelID]
	if !ok {
		return fmt.Errorf("no label %s", labelID)
	}
	row.TaskID = ""
	l.rows[labelID] = row
	return nil
}

// attachForeign models a label this package does not own.
func (l *fakeLabels) attachForeign(taskID, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	id := fmt.Sprintf("lbl-%d", l.nextID)
	l.rows[id] = provider.TaskLabel{ID: id, Name: name, TaskID: taskID}
}

func (l *fakeLabels) names(taskID string) []string {
	rows, _ := l.ListTaskLabels(context.Background(), taskID)
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func (l *fakeLabels) has(taskID, name string) bool {
	for _, n := range l.names(taskID) {
		if n == name {
			return true
		}
	}
	return false
}

type fakeAuthority struct {
	states map[string]*lifecycle.TaskState
	err    error
}

func (a *fakeAuthority) CurrentState(ref string) (*lifecycle.TaskState, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.states[ref], nil
}

// fakeReceipt stands in for the unmerged FAC-132 sync.CompletionReceipt. It
// has the same method set, so this test is exercising the real seam shape.
type fakeReceipt struct {
	err        error
	sawRepo    string
	sawRef     string
	sawState   *lifecycle.TaskState
	validateds int
}

func (r *fakeReceipt) Validate(repoDir, ref string, st *lifecycle.TaskState) error {
	r.sawRepo, r.sawRef, r.sawState = repoDir, ref, st
	r.validateds++
	return r.err
}

// ---------- harness ----------

type rig struct {
	t     *testing.T
	dir   string
	board *fakeBoard
	label *fakeLabels
	auth  *fakeAuthority
	p     *Projector
}

const (
	ref    = "FAC-143"
	taskID = "task-143"
	shaA   = "1111111111111111111111111111111111111111"
	shaB   = "2222222222222222222222222222222222222222"
)

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		t: t, dir: dir,
		board: newBoard(taskID, provider.StatusToDo),
		label: newLabels(),
		auth:  &fakeAuthority{states: map[string]*lifecycle.TaskState{}},
	}
	r.p = r.open()
	return r
}

// open builds a Projector over the SAME store path, so tests can simulate a
// process restart by closing and re-opening.
func (r *rig) open() *Projector {
	r.t.Helper()
	p, err := NewProjector(filepath.Join(r.dir, "boardproj.db"), r.board, r.label, r.auth, r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { p.Close() })
	return p
}

func (r *rig) restart() {
	r.t.Helper()
	if err := r.p.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.p = r.open()
}

func delivery(sha string, gen int64) *Delivery {
	return &Delivery{ReceiptKey: "deliver:" + sha, IntentSHA256: "sha256:" + sha, CandidateSHA: sha, Generation: gen}
}

// advance is the ordinary happy-path walk used to set a fixture up.
func (r *rig) advance(ev Event) Result {
	r.t.Helper()
	res, err := r.p.Apply(context.Background(), ev)
	if err != nil {
		r.t.Fatalf("apply %s -> %s: %v", ev.TaskRef, ev.To, err)
	}
	return res
}

func ev(to lifecycle.State, seq, gen int64) Event {
	return Event{TaskRef: ref, TaskID: taskID, To: to, Seq: seq, LeaseGeneration: gen, CandidateSHA: shaA}
}

// toReview walks a fresh card up to a live review at shaA/generation 1.
func (r *rig) toReview() {
	r.t.Helper()
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.advance(ev(lifecycle.StateClaimed, 2, 1))
	r.advance(ev(lifecycle.StateBuilding, 3, 1))
	e := ev(lifecycle.StateReviewing, 4, 1)
	e.Delivery = delivery(shaA, 1)
	r.advance(e)
}

// ---------- projection mapping ----------

func TestProjectCoversEveryLifecycleStateAndRejectsUnknown(t *testing.T) {
	want := map[lifecycle.State]Projection{
		lifecycle.StateDraft:             {Status: provider.StatusToDo},
		lifecycle.StateEligible:          {Status: provider.StatusToDo},
		lifecycle.StateClaimed:           {Status: provider.StatusInProgress},
		lifecycle.StateDispatched:        {Status: provider.StatusInProgress},
		lifecycle.StateBuilding:          {Status: provider.StatusInProgress},
		lifecycle.StateVerifying:         {Status: provider.StatusInProgress},
		lifecycle.StateReviewing:         {Status: provider.StatusInReview},
		lifecycle.StateIntegrationQueued: {Status: provider.StatusInReview},
		lifecycle.StateIntegrated:        {Status: provider.StatusInReview, DoneWithReceipt: true},
		lifecycle.StateReconciled:        {Status: provider.StatusInReview, DoneWithReceipt: true},
		lifecycle.StateCleaned:           {Status: provider.StatusInReview, DoneWithReceipt: true},
		lifecycle.StateBlocked:           {CarryForward: true, Label: LabelBlocked},
		lifecycle.StateRecovering:        {CarryForward: true, Label: LabelRecovering},
	}
	// NonTerminalStates + the terminal ones: assert we mapped the whole set,
	// so a state added to pkg/lifecycle without a projection fails here.
	all := append(lifecycle.NonTerminalStates(), lifecycle.StateCleaned)
	for _, s := range all {
		got, err := Project(s)
		if err != nil {
			t.Fatalf("state %s has no projection: %v", s, err)
		}
		exp, ok := want[s]
		if !ok {
			t.Fatalf("state %s is not covered by this test's expectations", s)
		}
		if got != exp {
			t.Errorf("Project(%s) = %+v, want %+v", s, got, exp)
		}
	}
	if len(want) != len(all) {
		t.Errorf("expectation table has %d states, lifecycle has %d", len(want), len(all))
	}
	if _, err := Project(lifecycle.State("shipped")); !errors.Is(err, ErrUnknownState) {
		t.Errorf("unknown state error = %v, want ErrUnknownState", err)
	}
}

func TestDeliveryRequiredOnlyForReviewEntryAndFailRepair(t *testing.T) {
	cases := []struct {
		prior, want string
		required    bool
	}{
		{provider.StatusInProgress, provider.StatusInReview, true}, // fresh review launch
		{provider.StatusToDo, provider.StatusInReview, true},
		{"", provider.StatusInReview, true},
		{provider.StatusInReview, provider.StatusInProgress, true}, // FAIL repair
		{provider.StatusInReview, provider.StatusInReview, false},
		{provider.StatusInReview, provider.StatusDone, false}, // gated by receipt, not delivery
		{provider.StatusToDo, provider.StatusInProgress, false},
		{provider.StatusInProgress, provider.StatusInProgress, false},
	}
	for _, c := range cases {
		if got := DeliveryRequired(c.prior, c.want); got != c.required {
			t.Errorf("DeliveryRequired(%q, %q) = %v, want %v", c.prior, c.want, got, c.required)
		}
	}
}

// ---------- AC: reviewer-live / delivered FAIL repair / visible dependency block ----------

func TestReviewerLiveCardIsInReviewOnlyWithExactSHADelivery(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.advance(ev(lifecycle.StateBuilding, 2, 1))

	// Without delivery proof, the card must NOT claim a reviewer is live.
	if _, err := r.p.Apply(context.Background(), ev(lifecycle.StateReviewing, 3, 1)); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("review with no delivery: err = %v, want ErrDeliveryUnconfirmed", err)
	}
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("refused review moved the card to %s", got)
	}

	// A delivery naming a DIFFERENT candidate is a stale handoff.
	stale := ev(lifecycle.StateReviewing, 3, 1)
	stale.Delivery = delivery(shaB, 1)
	if _, err := r.p.Apply(context.Background(), stale); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("review with wrong-SHA delivery: err = %v, want ErrDeliveryUnconfirmed", err)
	}
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("wrong-SHA delivery moved the card to %s", got)
	}

	// The exact-SHA delivery is what authorizes In Review.
	live := ev(lifecycle.StateReviewing, 3, 1)
	live.Delivery = delivery(shaA, 1)
	if res := r.advance(live); res.Status != provider.StatusInReview {
		t.Fatalf("result status = %s, want in-review", res.Status)
	}
	if got := r.board.status(taskID); got != provider.StatusInReview {
		t.Fatalf("board status = %s, want in-review", got)
	}
}

func TestFailRepairLeavesInReviewOnlyAfterRepairDelivery(t *testing.T) {
	r := newRig(t)
	r.toReview()

	// REJECT: lifecycle went reviewing -> building. Without the repair
	// prompt actually delivered, the card stays In Review (FAC-68/FAC-84).
	if _, err := r.p.Apply(context.Background(), ev(lifecycle.StateBuilding, 5, 1)); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("repair with no delivery: err = %v, want ErrDeliveryUnconfirmed", err)
	}
	if got := r.board.status(taskID); got != provider.StatusInReview {
		t.Fatalf("unproven repair moved the card to %s", got)
	}

	repair := ev(lifecycle.StateBuilding, 5, 1)
	repair.Delivery = delivery(shaA, 1)
	r.advance(repair)
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("board status after delivered repair = %s, want in-progress", got)
	}
}

func TestDependencyBlockIsVisibleWithExactReasonAndKeepsSafeColumn(t *testing.T) {
	r := newRig(t)
	r.toReview()

	blocked := ev(lifecycle.StateBlocked, 5, 1)
	blocked.Reason = Reason{
		Reason:     "waiting on an upstream card",
		Owner:      "forge-smith",
		Dependency: "FAC-119",
		NextEvent:  "FAC-119 integrated",
	}
	res := r.advance(blocked)

	if res.Label != LabelBlocked {
		t.Fatalf("label = %q, want %s", res.Label, LabelBlocked)
	}
	if !r.label.has(taskID, LabelBlocked) {
		t.Fatalf("card labels = %v, want %s attached", r.label.names(taskID), LabelBlocked)
	}
	// Blocked carries the column forward: a reviewer-live card that blocks
	// must not be demoted to In Progress, and must never reach Done.
	if got := r.board.status(taskID); got != provider.StatusInReview {
		t.Fatalf("blocked card column = %s, want the carried-forward in-review", got)
	}
	body := strings.Join(r.board.comments[taskID], "\n")
	for _, want := range []string{"waiting on an upstream card", "forge-smith", "FAC-119", "FAC-119 integrated"} {
		if !strings.Contains(body, want) {
			t.Errorf("state comment is missing %q; got:\n%s", want, body)
		}
	}
}

func TestManagedLabelsAreReplacedAndForeignLabelsAreNeverTouched(t *testing.T) {
	r := newRig(t)
	r.label.attachForeign(taskID, "needs-design")
	r.toReview()

	blocked := ev(lifecycle.StateBlocked, 5, 1)
	blocked.Reason = Reason{Dependency: "FAC-119"}
	r.advance(blocked)
	if !r.label.has(taskID, LabelBlocked) || r.label.has(taskID, LabelRecovering) {
		t.Fatalf("labels = %v, want exactly the blocked managed label", r.label.names(taskID))
	}

	r.advance(ev(lifecycle.StateRecovering, 6, 1))
	if r.label.has(taskID, LabelBlocked) || !r.label.has(taskID, LabelRecovering) {
		t.Fatalf("labels = %v, want blocked swapped for recovering", r.label.names(taskID))
	}
	if !r.label.has(taskID, "needs-design") {
		t.Fatalf("a foreign label was removed: %v", r.label.names(taskID))
	}
}

// ---------- AC: stale SHA / generation cannot move the card ----------

func TestStaleGenerationAndSequenceCannotMoveTheCard(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 2)) // claimed at generation 2
	r.advance(ev(lifecycle.StateBuilding, 2, 2))
	before := r.board.status(taskID)
	writes := r.board.writes

	// A superseded generation, arriving late.
	late := ev(lifecycle.StateReviewing, 3, 1)
	late.Delivery = delivery(shaA, 1)
	if _, err := r.p.Apply(context.Background(), late); !errors.Is(err, ErrStaleEvent) {
		t.Fatalf("stale generation: err = %v, want ErrStaleEvent", err)
	}

	// An event behind the applied sequence.
	if _, err := r.p.Apply(context.Background(), ev(lifecycle.StateEligible, 1, 2)); !errors.Is(err, ErrStaleEvent) {
		t.Fatalf("stale sequence: err = %v, want ErrStaleEvent", err)
	}

	// A delivery minted for a superseded generation cannot authorize review
	// even when the event's own generation is current.
	mixed := ev(lifecycle.StateReviewing, 3, 2)
	mixed.Delivery = delivery(shaA, 1)
	if _, err := r.p.Apply(context.Background(), mixed); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("stale-generation delivery: err = %v, want ErrDeliveryUnconfirmed", err)
	}

	if got := r.board.status(taskID); got != before {
		t.Fatalf("board moved to %s; stale events must not write", got)
	}
	if r.board.writes != writes {
		t.Fatalf("board writes went %d -> %d; a refused event must make zero writes", writes, r.board.writes)
	}
}

func TestCardIdentityDriftIsRefused(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	other := ev(lifecycle.StateBuilding, 2, 1)
	other.TaskID = "task-other"
	if _, err := r.p.Apply(context.Background(), other); !errors.Is(err, ErrCardIdentityDrift) {
		t.Fatalf("err = %v, want ErrCardIdentityDrift", err)
	}
}

// ---------- AC: dropped and duplicate events converge after restart ----------

func TestDuplicateEventIsReplayedWithoutASecondWriteOrComment(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.advance(ev(lifecycle.StateBuilding, 2, 1))
	writes, comments := r.board.writes, len(r.board.comments[taskID])

	r.restart() // the duplicate arrives after a crash

	res, err := r.p.Apply(context.Background(), ev(lifecycle.StateBuilding, 2, 1))
	if err != nil {
		t.Fatalf("duplicate after restart: %v", err)
	}
	if !res.Replayed {
		t.Fatalf("duplicate was not reported as a replay: %+v", res)
	}
	if r.board.writes != writes {
		t.Fatalf("duplicate wrote the board again: %d -> %d", writes, r.board.writes)
	}
	if got := len(r.board.comments[taskID]); got != comments {
		t.Fatalf("duplicate commented again: %d -> %d", comments, got)
	}
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("status after replay = %s, want in-progress", got)
	}
}

func TestConflictingProjectionAtTheSameSequenceIsRefused(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.advance(ev(lifecycle.StateBuilding, 2, 1))
	conflict := ev(lifecycle.StateReviewing, 2, 1)
	conflict.Delivery = delivery(shaA, 1)
	if _, err := r.p.Apply(context.Background(), conflict); !errors.Is(err, ErrStaleEvent) {
		t.Fatalf("err = %v, want ErrStaleEvent for a different projection at seq 2", err)
	}
}

func TestDroppedIntermediateEventsFoldForwardAfterRestart(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.restart()

	// seq 2 (claimed) and 3 (dispatched) were dropped; seq 4 arrives.
	res := r.advance(ev(lifecycle.StateBuilding, 4, 1))
	if res.Status != provider.StatusInProgress {
		t.Fatalf("folded-forward status = %s, want in-progress", res.Status)
	}
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("board = %s, want in-progress", got)
	}
	// And the dropped events, if they now arrive, still cannot move it back.
	if _, err := r.p.Apply(context.Background(), ev(lifecycle.StateClaimed, 2, 1)); !errors.Is(err, ErrStaleEvent) {
		t.Fatalf("late dropped event: err = %v, want ErrStaleEvent", err)
	}
}

// ---------- AC: Done requires the FAC-132 receipt and provider readback ----------

func TestIntegratedWithoutReceiptHoldsAtInReview(t *testing.T) {
	r := newRig(t)
	r.toReview()
	res := r.advance(ev(lifecycle.StateIntegrated, 5, 1))
	if res.Status != provider.StatusInReview {
		t.Fatalf("status = %s, want in-review with no completion receipt", res.Status)
	}
	if got := r.board.status(taskID); got == provider.StatusDone {
		t.Fatal("integrated with no receipt closed the card")
	}
}

func TestDoneRequiresAValidReceiptValidatedAgainstDurableLifecycleState(t *testing.T) {
	r := newRig(t)
	r.toReview()
	r.auth.states[ref] = &lifecycle.TaskState{TaskRef: ref, State: lifecycle.StateIntegrated, Seq: 5,
		LeaseGeneration: 1, CandidateSHA: shaA}

	// An invalid receipt is a hard error, never a quiet downgrade to In Review.
	bad := ev(lifecycle.StateIntegrated, 5, 1)
	bad.Receipt = &fakeReceipt{err: errors.New("patch id does not match origin/main")}
	if _, err := r.p.Apply(context.Background(), bad); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("invalid receipt: err = %v, want ErrReceiptInvalid", err)
	}
	if got := r.board.status(taskID); got == provider.StatusDone {
		t.Fatal("invalid receipt closed the card")
	}

	good := &fakeReceipt{}
	ok := ev(lifecycle.StateIntegrated, 5, 1)
	ok.Receipt = good
	if res := r.advance(ok); res.Status != provider.StatusDone {
		t.Fatalf("status = %s, want done", res.Status)
	}
	if got := r.board.status(taskID); got != provider.StatusDone {
		t.Fatalf("board = %s, want done", got)
	}
	// The receipt must have been validated against THIS repo, ref, and the
	// durable lifecycle state — not against anything the event carried.
	if good.validateds != 1 || good.sawRepo != r.dir || good.sawRef != ref {
		t.Fatalf("receipt validated %d times with repo=%q ref=%q", good.validateds, good.sawRepo, good.sawRef)
	}
	if good.sawState == nil || good.sawState.Seq != 5 || good.sawState.CandidateSHA != shaA {
		t.Fatalf("receipt saw lifecycle state %+v, want the authority's record", good.sawState)
	}
}

func TestDoneIsRefusedWhenTheReadbackDoesNotSayDone(t *testing.T) {
	r := newRig(t)
	r.toReview()
	r.auth.states[ref] = &lifecycle.TaskState{TaskRef: ref, State: lifecycle.StateIntegrated, Seq: 5}
	r.board.driftTo = provider.StatusInReview // write accepted, board still says in-review

	e := ev(lifecycle.StateIntegrated, 5, 1)
	e.Receipt = &fakeReceipt{}
	res, err := r.p.Apply(context.Background(), e)
	if !errors.Is(err, ErrRecovering) {
		t.Fatalf("err = %v, want ErrRecovering", err)
	}
	if !res.Recovering {
		t.Fatalf("result = %+v, want Recovering", res)
	}
}

// ---------- AC: write success + mismatched readback is a hard Recovering ----------

func TestReadbackDriftIsAHardRecoveringState(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.board.driftTo = provider.StatusToDo // provider accepts the write, reports the old value

	res, err := r.p.Apply(context.Background(), ev(lifecycle.StateBuilding, 2, 1))
	if !errors.Is(err, ErrRecovering) {
		t.Fatalf("err = %v, want ErrRecovering", err)
	}
	if !res.Recovering || res.Label != LabelRecovering {
		t.Fatalf("result = %+v, want a Recovering result labelled %s", res, LabelRecovering)
	}
	if !r.label.has(taskID, LabelRecovering) {
		t.Fatalf("labels = %v, want %s", r.label.names(taskID), LabelRecovering)
	}
	body := strings.Join(r.board.comments[taskID], "\n")
	if !strings.Contains(body, "readback drift") {
		t.Fatalf("state comment does not name the drift:\n%s", body)
	}

	// Recovering is durable: it survives a restart, and the same sequence may
	// be retried (that is repair, not a conflicting claim).
	r.restart()
	r.board.driftTo = ""
	if res := r.advance(ev(lifecycle.StateBuilding, 2, 1)); res.Recovering {
		t.Fatalf("retry after drift still recovering: %+v", res)
	}
	if r.label.has(taskID, LabelRecovering) {
		t.Fatalf("recovering label survived a clean retry: %v", r.label.names(taskID))
	}
}

func TestAProviderWriteErrorNeverRecordsAProjection(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.board.failNext = errors.New("provider 500")
	if _, err := r.p.Apply(context.Background(), ev(lifecycle.StateBuilding, 2, 1)); err == nil {
		t.Fatal("write error was swallowed")
	}
	// The projection must still be at seq 1, so the failed move can be retried.
	a, err := r.p.load(ref)
	if err != nil || a == nil {
		t.Fatalf("load: %v %+v", err, a)
	}
	if a.Seq != 1 || a.Status != provider.StatusToDo {
		t.Fatalf("projection advanced past a failed write: %+v", a)
	}
}

// ---------- AC: reconciliation reproduces and prevents the audited failures ----------

// The 2026-08-02 audit: reviewers were live and PRs existed, yet the board
// showed zero cards In Review. Reconcile must detect and repair that from
// durable truth alone.
func TestReconcileRepairsTheZeroInReviewBoard(t *testing.T) {
	r := newRig(t)
	r.toReview()
	r.auth.states[ref] = &lifecycle.TaskState{TaskRef: ref, State: lifecycle.StateReviewing, Seq: 4, LeaseGeneration: 1}

	// Something outside the projector knocked the card off In Review.
	r.board.tasks[taskID].Status = provider.StatusInProgress
	r.restart()

	drifts, err := r.p.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drifts) != 1 || drifts[0].Kind != "BOARD_DRIFT" || !drifts[0].Repaired {
		t.Fatalf("drifts = %+v, want one repaired BOARD_DRIFT", drifts)
	}
	if got := r.board.status(taskID); got != provider.StatusInReview {
		t.Fatalf("board = %s after reconcile, want in-review", got)
	}

	// A board that already agrees produces no finding and no write.
	writes := r.board.writes
	again, err := r.p.Reconcile(context.Background())
	if err != nil || len(again) != 0 {
		t.Fatalf("second reconcile: %v %+v", err, again)
	}
	if r.board.writes != writes {
		t.Fatalf("a clean reconcile wrote the board: %d -> %d", writes, r.board.writes)
	}
}

// The other half of the audit: FAC-68 and FAC-84 stayed In Review after a
// REJECT while their workers were repairing. Here the lifecycle log recorded
// the REJECT but the projection event never arrived. Reconcile cannot make
// that column move itself (it has no repair-delivery proof), so it must stop
// the card asserting a review that is not happening.
func TestReconcileMarksRejectedButStillInReviewCardsRecovering(t *testing.T) {
	r := newRig(t)
	r.toReview()
	r.auth.states[ref] = &lifecycle.TaskState{TaskRef: ref, State: lifecycle.StateBuilding, Seq: 5, LeaseGeneration: 1}
	r.restart()

	drifts, err := r.p.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drifts) != 1 || drifts[0].Kind != "LIFECYCLE_AHEAD" {
		t.Fatalf("drifts = %+v, want one LIFECYCLE_AHEAD", drifts)
	}
	if !strings.Contains(drifts[0].Detail, "seq 5") || !strings.Contains(drifts[0].Detail, "seq 4") {
		t.Fatalf("detail does not name both sequences: %q", drifts[0].Detail)
	}
	if !r.label.has(taskID, LabelRecovering) {
		t.Fatalf("labels = %v, want %s", r.label.names(taskID), LabelRecovering)
	}
	// Reconcile must NOT invent the In Review -> In Progress move.
	if got := r.board.status(taskID); got != provider.StatusInReview {
		t.Fatalf("reconcile moved the column to %s without delivery proof", got)
	}

	// The real repair event, with its delivery proof, still lands cleanly.
	repair := ev(lifecycle.StateBuilding, 5, 1)
	repair.Delivery = delivery(shaA, 1)
	r.advance(repair)
	if got := r.board.status(taskID); got != provider.StatusInProgress {
		t.Fatalf("board = %s after delivered repair, want in-progress", got)
	}
	if r.label.has(taskID, LabelRecovering) {
		t.Fatalf("recovering label survived the repair: %v", r.label.names(taskID))
	}
}

func TestReconcileReportsACardWithNoLifecycleState(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	// auth has no state for ref at all.
	drifts, err := r.p.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drifts) != 1 || drifts[0].Kind != "LIFECYCLE_MISSING" || drifts[0].Repaired {
		t.Fatalf("drifts = %+v, want one unrepaired LIFECYCLE_MISSING", drifts)
	}
}

func TestReconcileSurfacesAuthorityErrorsInsteadOfReportingAHealthyBoard(t *testing.T) {
	r := newRig(t)
	r.advance(ev(lifecycle.StateEligible, 1, 1))
	r.auth.err = errors.New("lifecycle store unavailable")
	drifts, err := r.p.Reconcile(context.Background())
	if err == nil {
		t.Fatal("reconcile reported success while it could not read lifecycle truth")
	}
	if len(drifts) != 0 {
		t.Fatalf("drifts = %+v, want none when truth is unreadable", drifts)
	}
}

func TestNewProjectorRequiresEveryDependency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")
	board, labels, auth := newBoard(taskID, provider.StatusToDo), newLabels(), &fakeAuthority{}
	cases := []struct {
		name string
		call func() (*Projector, error)
	}{
		{"board", func() (*Projector, error) { return NewProjector(path, nil, labels, auth, dir) }},
		{"labels", func() (*Projector, error) { return NewProjector(path, board, nil, auth, dir) }},
		{"authority", func() (*Projector, error) { return NewProjector(path, board, labels, nil, dir) }},
		{"repoDir", func() (*Projector, error) { return NewProjector(path, board, labels, auth, "") }},
	}
	for _, c := range cases {
		if _, err := c.call(); err == nil {
			t.Errorf("missing %s was accepted", c.name)
		}
	}
}
