package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/boardfreeze"
)

// ErrBoardFrozen is wrapped into every mutation BoundClient refuses while
// the FAC-103 board-freeze gate is active. Callers use errors.Is against
// this to distinguish a deliberate freeze refusal from a transport error.
var ErrBoardFrozen = errors.New("provider: board is frozen")

// freezeGate is consulted fresh (never cached) before every mutating call,
// so a caller that retries after a refusal always sees the live
// generation — a retry can never be judged against a freeze state that has
// since been superseded. Reads bypass this entirely.
func freezeGate(op string) error {
	st, frozen, err := boardfreeze.Active(time.Now())
	if err != nil {
		return fmt.Errorf("%s: BLOCKED(board_freeze_unavailable): %w", op, err)
	}
	if !frozen {
		return nil
	}
	_ = boardfreeze.RecordBlock() // best-effort accounting; refusal stands regardless
	return fmt.Errorf("%s: %w (actor=%q reason=%q scope=%q generation=%d)", op, ErrBoardFrozen, st.Actor, st.Reason, st.Scope, st.Generation)
}

// BoundClient is the production TaskProvider wrapper: every external board
// call gets a configured per-op deadline and timeout/ambiguous failures are
// labeled BLOCKED(provider_timeout) for control-plane consumers (FAC-150).
// Non-Kaneo live activation remains FAC-155; this type is provider-agnostic.
type BoundClient struct {
	Inner     TaskProvider
	Deadlines Deadlines
}

// NewBoundClient wraps inner with d (normalized). Inner must be non-nil.
func NewBoundClient(inner TaskProvider, d Deadlines) *BoundClient {
	if inner == nil {
		return nil
	}
	return &BoundClient{Inner: inner, Deadlines: d.Normalize()}
}

func (b *BoundClient) deadlines() Deadlines {
	if b == nil {
		return DefaultDeadlines()
	}
	return b.Deadlines.Normalize()
}

func (b *BoundClient) wrap(op string, kind OpKind, err error) error {
	if err == nil {
		return nil
	}
	// Preserve AmbiguousMutationError before AsTimeout (which would surface the
	// nested TimeoutError and drop the ambiguous classification).
	if IsAmbiguous(err) {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	err = AsTimeout("provider", op, kind, b.deadlines().For(kind), err)
	class := ClassifyOpError(err)
	if class == OpTimeout || class == OpAmbiguous {
		return fmt.Errorf("%s: BLOCKED(provider_timeout): %w", op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func (b *BoundClient) GetTask(ctx context.Context, id string) (*Task, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("GetTask: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpGet)
	defer cancel()
	t, err := b.Inner.GetTask(opCtx, id)
	return t, b.wrap("GetTask", OpGet, err)
}

func (b *BoundClient) ListTasks(ctx context.Context, projectID, status string) ([]*Task, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("ListTasks: nil provider")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpList)
	defer cancel()
	tasks, err := b.Inner.ListTasks(opCtx, projectID, status)
	if err != nil {
		return nil, b.wrap("ListTasks", OpList, err)
	}
	// Never treat timeout as empty success: nil err + nil/empty is fine;
	// a wrapped timeout already returned above.
	return tasks, nil
}

func (b *BoundClient) ClaimTask(ctx context.Context, taskID, role string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("ClaimTask: nil provider")
	}
	if err := freezeGate("ClaimTask"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("ClaimTask", OpMutate, b.Inner.ClaimTask(opCtx, taskID, role))
}

func (b *BoundClient) UpdateStatus(ctx context.Context, taskID, status string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("UpdateStatus: nil provider")
	}
	if err := freezeGate("UpdateStatus"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("UpdateStatus", OpMutate, b.Inner.UpdateStatus(opCtx, taskID, status))
}

func (b *BoundClient) AddComment(ctx context.Context, taskID, body string) error {
	if b == nil || b.Inner == nil {
		return fmt.Errorf("AddComment: nil provider")
	}
	if err := freezeGate("AddComment"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpComment)
	defer cancel()
	return b.wrap("AddComment", OpComment, b.Inner.AddComment(opCtx, taskID, body))
}

// Label operations are delegated only when the underlying adapter explicitly
// implements the label contract. This keeps old providers fail-closed while
// allowing the production daemon to reach Kaneo through BoundClient.
func (b *BoundClient) labelProvider() (TaskLabelProvider, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("label provider: nil")
	}
	p, ok := b.Inner.(TaskLabelProvider)
	if !ok {
		return nil, fmt.Errorf("label provider unsupported")
	}
	return p, nil
}

func (b *BoundClient) ListTaskLabels(ctx context.Context, taskID string) ([]TaskLabel, error) {
	p, err := b.labelProvider()
	if err != nil {
		return nil, err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpList)
	defer cancel()
	rows, e := p.ListTaskLabels(opCtx, taskID)
	return rows, b.wrap("ListTaskLabels", OpList, e)
}
func (b *BoundClient) CreateTaskLabel(ctx context.Context, taskID, name string) (TaskLabel, error) {
	p, err := b.labelProvider()
	if err != nil {
		return TaskLabel{}, err
	}
	if err := freezeGate("CreateTaskLabel"); err != nil {
		return TaskLabel{}, err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	row, e := p.CreateTaskLabel(opCtx, taskID, name)
	return row, b.wrap("CreateTaskLabel", OpMutate, e)
}
func (b *BoundClient) AttachTaskLabel(ctx context.Context, taskID, labelID string) error {
	p, err := b.labelProvider()
	if err != nil {
		return err
	}
	if err := freezeGate("AttachTaskLabel"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("AttachTaskLabel", OpMutate, p.AttachTaskLabel(opCtx, taskID, labelID))
}
func (b *BoundClient) DetachTaskLabel(ctx context.Context, labelID string) error {
	p, err := b.labelProvider()
	if err != nil {
		return err
	}
	if err := freezeGate("DetachTaskLabel"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("DetachTaskLabel", OpMutate, p.DetachTaskLabel(opCtx, labelID))
}
func (b *BoundClient) DeleteTaskLabel(ctx context.Context, labelID string) error {
	p, err := b.labelProvider()
	if err != nil {
		return err
	}
	if err := freezeGate("DeleteTaskLabel"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("DeleteTaskLabel", OpMutate, p.DeleteTaskLabel(opCtx, labelID))
}
func (b *BoundClient) ProveLabelCreation(ctx context.Context, created TaskLabel, targetID, name string, opts LabelRepairOptions) error {
	p, err := b.labelProvider()
	if err != nil {
		return err
	}
	proof, ok := p.(LabelCreationProof)
	if !ok {
		return fmt.Errorf("label creation proof unsupported")
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpReadback)
	defer cancel()
	return b.wrap("ProveLabelCreation", OpReadback, proof.ProveLabelCreation(opCtx, created, targetID, name, opts))
}
func (b *BoundClient) LabelMutationAuthority() (string, error) {
	p, err := b.labelProvider()
	if err != nil {
		return "", err
	}
	a, ok := p.(labelAuthority)
	if !ok {
		return "", fmt.Errorf("label authority unsupported")
	}
	return a.LabelMutationAuthority()
}

// ConfigDeadlineSource is the subset of config needed without importing
// pkg/config (avoids cycles). cmd and packages pass Resolved() parts.
type ConfigDeadlineSource interface {
	// Resolved returns get,list,mutate,comment,readback durations (0 = default).
	ResolvedDeadlines() (get, list, mutate, comment, readback time.Duration, err error)
}

// DeadlinesFromResolved maps Resolved() output to Deadlines.
func DeadlinesFromResolved(get, list, mutate, comment, readback time.Duration, err error) Deadlines {
	if err != nil {
		return DefaultDeadlines()
	}
	return DeadlinesFromParts(get, list, mutate, comment, readback)
}

// --- RelationProvider / BulkRelationProvider delegation (FAC-159) ------------

func (b *BoundClient) ListRelations(ctx context.Context, taskID string) ([]Relation, error) {
	rp, err := b.relationProvider()
	if err != nil {
		return nil, err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpList)
	defer cancel()
	rels, e := rp.ListRelations(opCtx, taskID)
	return rels, b.wrap("ListRelations", OpList, e)
}

func (b *BoundClient) CreateRelation(ctx context.Context, sourceID, targetID string, typ RelationType) (*Relation, error) {
	rp, err := b.relationProvider()
	if err != nil {
		return nil, err
	}
	if err := freezeGate("CreateRelation"); err != nil {
		return nil, err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	rel, e := rp.CreateRelation(opCtx, sourceID, targetID, typ)
	return rel, b.wrap("CreateRelation", OpMutate, e)
}

func (b *BoundClient) DeleteRelation(ctx context.Context, relationID, sourceID, targetID string) error {
	rp, err := b.relationProvider()
	if err != nil {
		return err
	}
	if err := freezeGate("DeleteRelation"); err != nil {
		return err
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpMutate)
	defer cancel()
	return b.wrap("DeleteRelation", OpMutate, rp.DeleteRelation(opCtx, relationID, sourceID, targetID))
}

func (b *BoundClient) ListProjectRelations(ctx context.Context, projectID string) ([]Relation, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("ListProjectRelations: nil provider")
	}
	bp, ok := b.Inner.(BulkRelationProvider)
	if !ok {
		// Fall through: if Inner is BoundClient-nested, unwrap once.
		if inner, ok2 := b.Inner.(*BoundClient); ok2 {
			return inner.ListProjectRelations(ctx, projectID)
		}
		return nil, fmt.Errorf("%w: inner does not implement BulkRelationProvider", errCapability)
	}
	opCtx, cancel := BoundOp(ctx, b.deadlines(), OpList)
	defer cancel()
	rels, e := bp.ListProjectRelations(opCtx, projectID)
	return rels, b.wrap("ListProjectRelations", OpList, e)
}

func (b *BoundClient) relationProvider() (RelationProvider, error) {
	if b == nil || b.Inner == nil {
		return nil, fmt.Errorf("relation provider: nil")
	}
	if rp, ok := b.Inner.(RelationProvider); ok {
		return rp, nil
	}
	return nil, fmt.Errorf("%w: inner does not implement RelationProvider", errCapability)
}

// errCapability is a local sentinel for missing relation capability on BoundClient.
var errCapability = fmt.Errorf("provider relation capability unsupported")

// Ensure BoundClient implements TaskProvider + relation surfaces when Inner does.
var (
	_ TaskProvider = (*BoundClient)(nil)
)
