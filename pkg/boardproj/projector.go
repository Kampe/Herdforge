package boardproj

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"

	_ "modernc.org/sqlite"
)

var (
	// ErrUnknownState means a lifecycle state has no taught projection.
	ErrUnknownState = errors.New("boardproj: unknown lifecycle state")
	// ErrStaleEvent means the event is older than, or conflicts with, the
	// projection already durably applied for that card.
	ErrStaleEvent = errors.New("boardproj: event is stale for the applied projection")
	// ErrDeliveryUnconfirmed means a column move that requires proof of an
	// exact-SHA prompt delivery did not get it.
	ErrDeliveryUnconfirmed = errors.New("boardproj: column move requires a confirmed exact-SHA delivery")
	// ErrReceiptInvalid means a completion receipt was supplied for Done but
	// did not validate. A wrong Done claim is a hard error, never a fallback
	// to In Review: silently downgrading it would hide a forged receipt.
	ErrReceiptInvalid = errors.New("boardproj: completion receipt did not validate")
	// ErrRecovering means the provider accepted a write but read back
	// something else. The card is left labelled Recovering.
	ErrRecovering = errors.New("boardproj: provider readback drifted from the write")
	// ErrCardIdentityDrift means the event names a different provider task id
	// than the one this ref was previously projected onto.
	ErrCardIdentityDrift = errors.New("boardproj: event task id differs from the applied projection")
)

// LifecycleAuthority is the durable task-state read model. *lifecycle.EventStore
// satisfies it. Nothing else is consulted for what state a task is in.
type LifecycleAuthority interface {
	CurrentState(taskRef string) (*lifecycle.TaskState, error)
}

// CompletionReceipt is the FAC-132 seam this package consumes for the Done
// gate. It is deliberately an interface declared here rather than an import:
// FAC-132 (commit "feat(donereceipt): task-bound completion receipt gates
// board Done") is NOT on origin/main as of this commit, and inventing a
// concrete type to call would be worse than naming the seam.
//
// The exact symbol being waited on is sync.CompletionReceipt, whose method is
//
//	func (r CompletionReceipt) Validate(repoDir, ref string, st *lifecycle.TaskState) error
//
// declared with a value receiver, so both sync.CompletionReceipt and
// *sync.CompletionReceipt satisfy this interface unchanged once FAC-132 lands.
// Until then a caller may pass any receipt implementation, and passing none
// simply holds the card at In Review.
type CompletionReceipt interface {
	Validate(repoDir, ref string, st *lifecycle.TaskState) error
}

// LabelBoard is the managed-label surface. provider.TaskLabelProvider (which
// *provider.KaneoProvider implements) satisfies it. DeleteTaskLabel is
// deliberately excluded: this package attaches and detaches, it never destroys
// a workspace label.
type LabelBoard interface {
	ListTaskLabels(ctx context.Context, taskID string) ([]provider.TaskLabel, error)
	CreateTaskLabel(ctx context.Context, taskID, name string) (provider.TaskLabel, error)
	AttachTaskLabel(ctx context.Context, taskID, labelID string) error
	DetachTaskLabel(ctx context.Context, labelID string) error
}

// Event is one durable lifecycle transition offered for projection. Seq and
// LeaseGeneration come from lifecycle.Event; they are what makes a stale or
// duplicate delivery harmless.
type Event struct {
	TaskRef         string
	TaskID          string
	To              lifecycle.State
	Seq             int64
	LeaseGeneration int64
	CandidateSHA    string
	Reason          Reason
	Delivery        *Delivery
	Receipt         CompletionReceipt
}

// Result reports what a projection did.
type Result struct {
	Ref        string
	TaskID     string
	Status     string
	Label      string
	Replayed   bool
	Recovering bool
	Commented  bool
}

// applied is the durable record of the last projection written for a card.
type applied struct {
	TaskRef         string
	TaskID          string
	Seq             int64
	LeaseGeneration int64
	State           string
	Status          string
	Label           string
	CommentDigest   string
	CandidateSHA    string
	Health          string
}

const (
	healthOK         = "ok"
	healthRecovering = "recovering"
)

// Projector writes lifecycle projections onto a board and can re-derive them
// after a restart.
type Projector struct {
	db        *sql.DB
	board     provider.TaskProvider
	labels    LabelBoard
	authority LifecycleAuthority
	repoDir   string
	now       func() time.Time
}

// NewProjector opens (or creates) the projection store at dbPath. Every
// dependency is required: a projector that cannot read lifecycle truth, write
// the board, manage its labels, or persist what it applied cannot be honest
// about any of them, so there is no degraded mode.
func NewProjector(dbPath string, board provider.TaskProvider, labels LabelBoard, authority LifecycleAuthority, repoDir string) (*Projector, error) {
	if board == nil || labels == nil || authority == nil {
		return nil, errors.New("boardproj: board, labels, and lifecycle authority are all required")
	}
	if repoDir == "" {
		return nil, errors.New("boardproj: repoDir is required for receipt validation")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("boardproj: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("boardproj: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS board_projections (
		task_ref TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		lease_generation INTEGER NOT NULL,
		state TEXT NOT NULL,
		status TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		comment_digest TEXT NOT NULL DEFAULT '',
		candidate_sha TEXT NOT NULL DEFAULT '',
		health TEXT NOT NULL DEFAULT 'ok',
		updated_at DATETIME NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("boardproj: migrate: %w", err)
	}
	return &Projector{db: db, board: board, labels: labels, authority: authority, repoDir: repoDir, now: time.Now}, nil
}

func (p *Projector) Close() error { return p.db.Close() }

// SetClock is for deterministic tests.
func (p *Projector) SetClock(now func() time.Time) { p.now = now }

func (p *Projector) load(ref string) (*applied, error) {
	row := p.db.QueryRow(`SELECT task_ref, task_id, seq, lease_generation, state, status,
		label, comment_digest, candidate_sha, health FROM board_projections WHERE task_ref = ?`, ref)
	var a applied
	err := row.Scan(&a.TaskRef, &a.TaskID, &a.Seq, &a.LeaseGeneration, &a.State, &a.Status,
		&a.Label, &a.CommentDigest, &a.CandidateSHA, &a.Health)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("boardproj: load projection: %w", err)
	}
	return &a, nil
}

func (p *Projector) save(a applied) error {
	_, err := p.db.Exec(`INSERT INTO board_projections
		(task_ref, task_id, seq, lease_generation, state, status, label, comment_digest, candidate_sha, health, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_ref) DO UPDATE SET
			task_id=excluded.task_id, seq=excluded.seq, lease_generation=excluded.lease_generation,
			state=excluded.state, status=excluded.status, label=excluded.label,
			comment_digest=excluded.comment_digest, candidate_sha=excluded.candidate_sha,
			health=excluded.health, updated_at=excluded.updated_at`,
		a.TaskRef, a.TaskID, a.Seq, a.LeaseGeneration, a.State, a.Status, a.Label,
		a.CommentDigest, a.CandidateSHA, a.Health, p.now().UTC())
	if err != nil {
		return fmt.Errorf("boardproj: save projection: %w", err)
	}
	return nil
}

// Apply projects one lifecycle event onto the board.
//
// Order matters and is deliberate. Everything that can refuse does so BEFORE
// the provider is touched, so a refused projection makes zero board writes:
// staleness, card identity, delivery proof and receipt validity are all
// settled first. Only then is the status written, read back, and — only if
// the readback agrees — recorded as applied.
func (p *Projector) Apply(ctx context.Context, ev Event) (Result, error) {
	if ev.TaskRef == "" || ev.TaskID == "" {
		return Result{}, errors.New("boardproj: task ref and provider task id are required")
	}
	if ev.Seq <= 0 || ev.LeaseGeneration <= 0 {
		return Result{}, errors.New("boardproj: positive seq and lease generation are required")
	}

	proj, err := Project(ev.To)
	if err != nil {
		return Result{}, err
	}

	prior, err := p.load(ev.TaskRef)
	if err != nil {
		return Result{}, err
	}
	priorStatus := ""
	if prior != nil {
		if prior.TaskID != ev.TaskID {
			return Result{}, fmt.Errorf("%w: %s was projected onto %s, event names %s",
				ErrCardIdentityDrift, ev.TaskRef, prior.TaskID, ev.TaskID)
		}
		// A superseded lease generation can never move the card, however late
		// it arrives; nor can an event behind the applied sequence.
		if ev.LeaseGeneration < prior.LeaseGeneration {
			return Result{}, fmt.Errorf("%w: %s applied generation %d, event is %d",
				ErrStaleEvent, ev.TaskRef, prior.LeaseGeneration, ev.LeaseGeneration)
		}
		if ev.Seq < prior.Seq {
			return Result{}, fmt.Errorf("%w: %s applied seq %d, event is %d",
				ErrStaleEvent, ev.TaskRef, prior.Seq, ev.Seq)
		}
		priorStatus = prior.Status
	}

	// Resolve the target status.
	status := proj.Status
	if proj.CarryForward {
		status = carryForward(priorStatus)
	}
	if proj.DoneWithReceipt && ev.Receipt != nil {
		st, err := p.authority.CurrentState(ev.TaskRef)
		if err != nil {
			return Result{}, fmt.Errorf("boardproj: read lifecycle state for %s: %w", ev.TaskRef, err)
		}
		if err := ev.Receipt.Validate(p.repoDir, ev.TaskRef, st); err != nil {
			return Result{}, fmt.Errorf("%w: %s: %v", ErrReceiptInvalid, ev.TaskRef, err)
		}
		status = provider.StatusDone
	}

	// A duplicate delivery of an event already applied is a no-op, not a
	// second write and not a second comment. A DIFFERENT projection at the
	// same sequence is a conflict, because one of the two is fabricated.
	if prior != nil && ev.Seq == prior.Seq {
		if prior.State == string(ev.To) && prior.Status == status && prior.Label == proj.Label && prior.Health == healthOK {
			return Result{Ref: ev.TaskRef, TaskID: ev.TaskID, Status: prior.Status, Label: prior.Label, Replayed: true}, nil
		}
		if prior.Health == healthOK {
			return Result{}, fmt.Errorf("%w: %s seq %d already applied as %s/%s, event wants %s/%s",
				ErrStaleEvent, ev.TaskRef, ev.Seq, prior.State, prior.Status, ev.To, status)
		}
		// A card left Recovering by a failed readback is allowed to retry the
		// same sequence: that is the repair path, not a conflicting claim.
	}

	if DeliveryRequired(priorStatus, status) {
		if err := ev.Delivery.confirms(ev.CandidateSHA, ev.LeaseGeneration); err != nil {
			return Result{}, fmt.Errorf("%s %s -> %s: %w", ev.TaskRef, provider.NormalizeStatus(priorStatus), status, err)
		}
	}

	next := applied{
		TaskRef: ev.TaskRef, TaskID: ev.TaskID, Seq: ev.Seq,
		LeaseGeneration: ev.LeaseGeneration, State: string(ev.To), Status: status,
		Label: proj.Label, CandidateSHA: ev.CandidateSHA, Health: healthOK,
	}
	if prior != nil {
		next.CommentDigest = prior.CommentDigest
	}

	if err := p.board.UpdateStatus(ctx, ev.TaskID, status); err != nil {
		return Result{}, fmt.Errorf("boardproj: write status for %s: %w", ev.TaskRef, err)
	}
	if err := p.verifyReadback(ctx, ev.TaskID, status); err != nil {
		// Write accepted, readback disagrees: the board's real state is
		// unknown. Record Recovering durably FIRST so a crash here cannot
		// leave the card silently claiming the status we failed to confirm.
		next.Health = healthRecovering
		next.Label = LabelRecovering
		saveErr := p.save(next)
		_, labelErr := p.reconcileLabels(ctx, ev.TaskID, LabelRecovering)
		_, commentErr := p.comment(ctx, &next, ev, "readback drift: "+err.Error())
		// Second save persists the comment digest, so a retry of the same
		// drift does not post the same comment again.
		digestErr := p.save(next)
		return Result{Ref: ev.TaskRef, TaskID: ev.TaskID, Status: status, Label: LabelRecovering, Recovering: true},
			errors.Join(fmt.Errorf("%w: %s: %v", ErrRecovering, ev.TaskRef, err), saveErr, labelErr, commentErr, digestErr)
	}

	if _, err := p.reconcileLabels(ctx, ev.TaskID, proj.Label); err != nil {
		return Result{}, fmt.Errorf("boardproj: reconcile labels for %s: %w", ev.TaskRef, err)
	}
	commented, err := p.comment(ctx, &next, ev, "")
	if err != nil {
		return Result{}, err
	}
	if err := p.save(next); err != nil {
		return Result{}, err
	}
	return Result{Ref: ev.TaskRef, TaskID: ev.TaskID, Status: status, Label: proj.Label, Commented: commented}, nil
}

func (p *Projector) verifyReadback(ctx context.Context, taskID, want string) error {
	got, err := p.board.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	if got == nil {
		return fmt.Errorf("readback: task %s not found after write", taskID)
	}
	return provider.VerifyStatusReadback(taskID, want, got.Status)
}

// reconcileLabels makes the card's managed labels exactly {want} (or empty).
// Labels outside ManagedLabels are never inspected for removal.
func (p *Projector) reconcileLabels(ctx context.Context, taskID, want string) (bool, error) {
	rows, err := p.labels.ListTaskLabels(ctx, taskID)
	if err != nil {
		return false, err
	}
	changed := false
	present := false
	for _, row := range rows {
		if !isManagedLabel(row.Name) {
			continue
		}
		if row.Name == want {
			present = true
			continue
		}
		if err := p.labels.DetachTaskLabel(ctx, row.ID); err != nil {
			return changed, fmt.Errorf("detach %s: %w", row.Name, err)
		}
		changed = true
	}
	if want == "" || present {
		return changed, nil
	}
	created, err := p.labels.CreateTaskLabel(ctx, taskID, want)
	if err != nil {
		return changed, fmt.Errorf("create %s: %w", want, err)
	}
	if err := p.labels.AttachTaskLabel(ctx, taskID, created.ID); err != nil {
		return changed, fmt.Errorf("attach %s: %w", want, err)
	}
	return true, nil
}

const commentMarker = "<!-- herd:lifecycle-state -->"

// stateComment renders the single idempotent state comment. Its content is a
// pure function of the projection, so an unchanged projection produces an
// unchanged digest and no second comment.
func stateComment(a *applied, ev Event, note string) (body, digest string) {
	var b strings.Builder
	b.WriteString(commentMarker + "\n")
	fmt.Fprintf(&b, "**Herdforge lifecycle** — state `%s`, board `%s`, seq %d, generation %d\n",
		a.State, a.Status, a.Seq, a.LeaseGeneration)
	if a.Label != "" {
		fmt.Fprintf(&b, "- label: `%s`\n", a.Label)
	}
	if a.CandidateSHA != "" {
		fmt.Fprintf(&b, "- candidate: `%s`\n", a.CandidateSHA)
	}
	if !ev.Reason.empty() {
		for _, f := range []struct{ k, v string }{
			{"reason", ev.Reason.Reason},
			{"owner", ev.Reason.Owner},
			{"dependency", ev.Reason.Dependency},
			{"next event", ev.Reason.NextEvent},
		} {
			if f.v != "" {
				fmt.Fprintf(&b, "- %s: %s\n", f.k, f.v)
			}
		}
	}
	if note != "" {
		fmt.Fprintf(&b, "- note: %s\n", note)
	}
	// Digest covers exactly what the comment asserts, so idempotency and
	// content can never disagree.
	payload, _ := json.Marshal(struct {
		State, Status, Label, SHA, Note string
		Seq, Generation                 int64
		Reason                          Reason
	}{a.State, a.Status, a.Label, a.CandidateSHA, note, a.Seq, a.LeaseGeneration, ev.Reason})
	sum := sha256.Sum256(payload)
	digest = "sha256:" + hex.EncodeToString(sum[:])
	fmt.Fprintf(&b, "<!-- herd:digest=%s -->", digest)
	return b.String(), digest
}

// comment posts the state comment only when its content actually changed.
func (p *Projector) comment(ctx context.Context, a *applied, ev Event, note string) (bool, error) {
	body, digest := stateComment(a, ev, note)
	if a.CommentDigest == digest {
		return false, nil
	}
	if err := p.board.AddComment(ctx, a.TaskID, body); err != nil {
		return false, fmt.Errorf("boardproj: comment on %s: %w", a.TaskRef, err)
	}
	a.CommentDigest = digest
	return true, nil
}

// Drift is one card whose board state does not match durable truth.
type Drift struct {
	Ref      string
	TaskID   string
	Kind     string // BOARD_DRIFT, LIFECYCLE_AHEAD, LIFECYCLE_MISSING
	Was      string
	Now      string
	Detail   string
	Repaired bool
}

// Reconcile re-derives every projected card from durable truth after a
// restart. It never invents authority: it can only re-assert a projection
// that was already durably authorized, or mark a card Recovering when the
// lifecycle log has moved past what the board was ever told.
//
// Three findings, matching the three ways the 2026-08-02 board lied:
//
//   - BOARD_DRIFT: the board disagrees with the projection we durably applied
//     (the zero-In-Review case: reviewers live, cards not In Review). The
//     applied projection is re-written and read back.
//   - LIFECYCLE_AHEAD: the lifecycle log is at a higher sequence than the
//     board was ever told (the rejected-but-In-Review case: the REJECT
//     advanced the log, the projection event was dropped). Reconcile CANNOT
//     make that move itself — leaving In Review needs the repair delivery
//     proof only Apply's caller holds — so it marks the card Recovering
//     rather than leaving it asserting a review that is not happening.
//   - LIFECYCLE_MISSING: a projected card with no lifecycle state at all.
//     Reported, never repaired: there is nothing to derive from.
func (p *Projector) Reconcile(ctx context.Context) ([]Drift, error) {
	rows, err := p.db.Query(`SELECT task_ref, task_id, seq, lease_generation, state, status,
		label, comment_digest, candidate_sha, health FROM board_projections ORDER BY task_ref ASC`)
	if err != nil {
		return nil, fmt.Errorf("boardproj: list projections: %w", err)
	}
	var cards []applied
	for rows.Next() {
		var a applied
		if err := rows.Scan(&a.TaskRef, &a.TaskID, &a.Seq, &a.LeaseGeneration, &a.State, &a.Status,
			&a.Label, &a.CommentDigest, &a.CandidateSHA, &a.Health); err != nil {
			rows.Close()
			return nil, fmt.Errorf("boardproj: scan projection: %w", err)
		}
		cards = append(cards, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boardproj: list projections: %w", err)
	}

	var drifts []Drift
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, card := range cards {
		st, err := p.authority.CurrentState(card.TaskRef)
		if err != nil {
			fail(fmt.Errorf("boardproj: reconcile %s: %w", card.TaskRef, err))
			continue
		}
		if st == nil {
			drifts = append(drifts, Drift{Ref: card.TaskRef, TaskID: card.TaskID, Kind: "LIFECYCLE_MISSING",
				Was: card.Status, Detail: "card is projected but has no durable lifecycle state"})
			continue
		}

		if st.Seq > card.Seq {
			d := Drift{Ref: card.TaskRef, TaskID: card.TaskID, Kind: "LIFECYCLE_AHEAD", Was: card.Status,
				Detail: fmt.Sprintf("lifecycle is at seq %d (%s), board was last told seq %d (%s)",
					st.Seq, st.State, card.Seq, card.State)}
			repaired, err := p.markRecovering(ctx, card, d.Detail)
			if err != nil {
				fail(err)
			}
			d.Repaired = repaired
			d.Now = card.Status
			drifts = append(drifts, d)
			continue
		}

		task, err := p.board.GetTask(ctx, card.TaskID)
		if err != nil {
			fail(fmt.Errorf("boardproj: reconcile read %s: %w", card.TaskRef, err))
			continue
		}
		live := ""
		if task != nil {
			live = task.Status
		}
		if provider.NormalizeStatus(live) == provider.NormalizeStatus(card.Status) {
			continue
		}
		d := Drift{Ref: card.TaskRef, TaskID: card.TaskID, Kind: "BOARD_DRIFT", Was: live, Now: card.Status,
			Detail: fmt.Sprintf("board says %s, durable projection says %s",
				provider.NormalizeStatus(live), card.Status)}
		if err := p.board.UpdateStatus(ctx, card.TaskID, card.Status); err != nil {
			fail(fmt.Errorf("boardproj: reconcile write %s: %w", card.TaskRef, err))
			drifts = append(drifts, d)
			continue
		}
		if err := p.verifyReadback(ctx, card.TaskID, card.Status); err != nil {
			fail(fmt.Errorf("%w: %s: %v", ErrRecovering, card.TaskRef, err))
			if _, mErr := p.markRecovering(ctx, card, "readback drift during reconcile: "+err.Error()); mErr != nil {
				fail(mErr)
			}
			drifts = append(drifts, d)
			continue
		}
		d.Repaired = true
		drifts = append(drifts, d)
	}

	sort.Slice(drifts, func(i, j int) bool { return drifts[i].Ref < drifts[j].Ref })
	return drifts, firstErr
}

// markRecovering labels a card Recovering and records why, without moving its
// column: the column it is on is the last one durable truth authorized, and
// reconcile has no authority to pick a new one.
func (p *Projector) markRecovering(ctx context.Context, card applied, detail string) (bool, error) {
	next := card
	next.Health = healthRecovering
	next.Label = LabelRecovering
	ev := Event{TaskRef: card.TaskRef, TaskID: card.TaskID, Reason: Reason{
		Reason:    detail,
		NextEvent: "a fresh lifecycle projection event carrying its delivery proof",
	}}
	if _, err := p.reconcileLabels(ctx, card.TaskID, LabelRecovering); err != nil {
		return false, fmt.Errorf("boardproj: recovering label for %s: %w", card.TaskRef, err)
	}
	if _, err := p.comment(ctx, &next, ev, ""); err != nil {
		return false, err
	}
	if err := p.save(next); err != nil {
		return false, err
	}
	return true, nil
}
