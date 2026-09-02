package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TaskLabel is an ownership-bearing label row. A task-bound label must never
// be attached to a different task; an empty TaskID is unknown ownership and is
// therefore unsafe for mutation.
type TaskLabel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	TaskID string `json:"taskId,omitempty"`
}

// LabelRepairEvidence is the durable transaction record for a role-label
// mutation.  It is deliberately provider-neutral so the daemon can persist it
// without depending on a particular board adapter.
type LabelRepairEvidence struct {
	Repository         string
	Provider           string
	Project            string
	SourceTaskID       string
	TargetTaskID       string
	PreSourceLabels    string
	PostSourceLabels   string
	PreTargetLabels    string
	PostTargetLabels   string
	PreSourceSnapshot  string
	PostSourceSnapshot string
	PreTargetSnapshot  string
	PostTargetSnapshot string
	CanonicalRole      string
	TransactionID      string
	Generation         string
	Outcome            string
	BlockedReason      string
	Phase              string
	Revision           string
	Operation          string
	CreatedLabelID     string
}

type LabelEvidenceSink interface {
	RecordLabelRepairEvidence(LabelRepairEvidence) error
}

type LabelEvidenceReader interface {
	ReadLabelRepairEvidence(transactionID, generation, phase string) (LabelRepairEvidence, error)
}

type LabelRepairOptions struct {
	Repository string
	Provider   string
	Project    string
	Evidence   LabelEvidenceSink
	// TransactionID and Generation are caller-owned identities. Empty values
	// are filled with a unique local transaction identity by the repair path.
	TransactionID string
	Revision      string
	Operation     string
	Generation    string
}

// LabelCreationProof is required before a returned create identity may be
// used for attach or compensation. Backends that cannot prove generation and
// transaction ownership fail closed rather than risking a foreign row.
type LabelCreationProof interface {
	ProveLabelCreation(context.Context, TaskLabel, string, string, LabelRepairOptions) error
}

// TaskLabelProvider is deliberately separate from TaskProvider: old adapters
// cannot accidentally claim to support destructive label operations.
type TaskLabelProvider interface {
	GetTask(context.Context, string) (*Task, error)
	ListTaskLabels(context.Context, string) ([]TaskLabel, error)
	CreateTaskLabel(context.Context, string, string) (TaskLabel, error)
	AttachTaskLabel(context.Context, string, string) error
	DetachTaskLabel(context.Context, string) error
	DeleteTaskLabel(context.Context, string) error
}

// WorkspaceLabelReader resolves one workspace label row by identity. It is
// optional so adapters without a workspace-wide label projection stay
// compatible. Where it is implemented, a row this transaction believes it just
// created is proven unattached before it is used as a donor: `kaneo label
// create` is idempotent by name and has been observed returning a
// three-week-old row instead of creating one, so "create fresh, then attach" is
// only safe once the returned row is known to belong to nobody.
type WorkspaceLabelReader interface {
	LookupWorkspaceLabel(context.Context, string) (TaskLabel, bool, error)
}

// assertDonorUnattached fails closed when the row is missing, unreadable, or
// held by a task other than the target. A caller that gets an error here must
// not detach or delete the row: it is somebody else's live label.
func assertDonorUnattached(ctx context.Context, p TaskLabelProvider, created TaskLabel, targetID string) error {
	reader, ok := p.(WorkspaceLabelReader)
	if !ok {
		return nil
	}
	row, found, err := reader.LookupWorkspaceLabel(ctx, created.ID)
	if err != nil {
		return fmt.Errorf("donor row %q unreadable: %w", created.ID, err)
	}
	if !found {
		return fmt.Errorf("donor row %q absent from workspace", created.ID)
	}
	if row.TaskID != "" && row.TaskID != targetID {
		return fmt.Errorf("%w: donor row %q is held by task %q, label create returned a live row", ErrLabelOwnershipUnknown, created.ID, row.TaskID)
	}
	return nil
}

// BulkTaskLabels is the result of a board-wide label read. Complete is based
// on the requested task identities, not on whether the provider returned an
// error. Consumers must check it before using Labels for board analytics.
// Truncated is kept as an explicit positive marker for JSON/reporting paths
// where a missing task would otherwise look like an empty label set.
type BulkTaskLabels struct {
	Labels    map[string][]TaskLabel `json:"labels"`
	Requested int                    `json:"requested"`
	Retrieved int                    `json:"retrieved"`
	Complete  bool                   `json:"complete"`
	Truncated bool                   `json:"truncated"`
}

// BulkTaskLabelProvider is optional so providers without a native task-list
// label projection remain compatible and fail closed at the capability
// boundary. The input identities may be provider IDs or human-readable refs;
// the result preserves each requested identity as a map key.
type BulkTaskLabelProvider interface {
	ListTaskLabelsBulk(context.Context, []string) (BulkTaskLabels, error)
}

var (
	ErrLabelOwnershipUnknown   = errors.New("label ownership unknown")
	ErrLabelTransactionBlocked = errors.New("label transaction durably blocked")
	ErrLabelGenerationMismatch = errors.New("label generation mismatch")
)

// LabelTransactionError means compensation could not establish the original
// source and target state. Callers must persist this as BLOCKED; it is never a
// successful repair.
type LabelTransactionError struct {
	Cause        error
	Compensation error
}

func (e *LabelTransactionError) Error() string {
	return fmt.Sprintf("%v: cause=%v compensation=%v", ErrLabelTransactionBlocked, e.Cause, e.Compensation)
}
func (e *LabelTransactionError) Unwrap() error { return ErrLabelTransactionBlocked }

var labelRepairLocks sync.Map // task id -> *sync.Mutex

type labelFence struct {
	files             []*os.File
	owner, generation string
}

type labelFenceMeta struct {
	Owner      string `json:"owner"`
	Generation string `json:"generation"`
	Sequence   int64  `json:"sequence"`
}

type labelAuthority interface{ LabelMutationAuthority() (string, error) }

func acquireLabelFence(p TaskLabelProvider, ids ...string) (*labelFence, error) {
	if len(ids) == 0 || strings.TrimSpace(ids[len(ids)-1]) == "" {
		return nil, fmt.Errorf("label fence: target identity required")
	}
	a, ok := p.(labelAuthority)
	if !ok {
		return nil, fmt.Errorf("label fence: canonical provider authority required")
	}
	authority, err := a.LabelMutationAuthority()
	if err != nil || authority == "" {
		return nil, fmt.Errorf("label fence: authority unavailable: %w", err)
	}
	root, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("label fence: canonical cache unavailable: %w", err)
	}
	dir := filepath.Join(root, "herdforge", "label-fences")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ownerBytes := make([]byte, 12)
	if _, err := rand.Read(ownerBytes); err != nil {
		return nil, fmt.Errorf("label fence: owner identity: %w", err)
	}
	owner := fmt.Sprintf("pid:%d:%x", os.Getpid(), ownerBytes)
	generation := fmt.Sprintf("%d", time.Now().UnixNano())
	meta := labelFenceMeta{Owner: owner, Generation: generation, Sequence: time.Now().UnixNano()}
	b, _ := json.Marshal(meta)
	keys := []string{authority + "|target|" + ids[len(ids)-1]}
	if len(ids) >= 2 {
		pair := []string{ids[0], ids[1]}
		sort.Strings(pair)
		keys = append(keys, authority+"|pair|"+pair[0]+"|"+pair[1])
	}
	sort.Strings(keys)
	fence := &labelFence{owner: owner, generation: meta.Generation}
	for _, key := range keys {
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		path := filepath.Join(dir, fmt.Sprintf("%016x.lock", h.Sum64()))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			if releaseErr := fence.release(); releaseErr != nil {
				return nil, fmt.Errorf("%w; fence cleanup: %v", err, releaseErr)
			}
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			closeErr := f.Close()
			releaseErr := fence.release()
			if releaseErr != nil {
				return nil, fmt.Errorf("%w; fence cleanup: %v", err, releaseErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("%w; close: %v", err, closeErr)
			}
			return nil, err
		}
		if err := f.Truncate(0); err != nil {
			closeErr := f.Close()
			releaseErr := fence.release()
			if releaseErr != nil {
				return nil, fmt.Errorf("%w; fence cleanup: %v", err, releaseErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("%w; close: %v", err, closeErr)
			}
			return nil, err
		}
		if _, err := f.Write(b); err != nil {
			closeErr := f.Close()
			releaseErr := fence.release()
			if releaseErr != nil {
				return nil, fmt.Errorf("%w; fence cleanup: %v", err, releaseErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("%w; close: %v", err, closeErr)
			}
			return nil, err
		}
		if err := f.Sync(); err != nil {
			closeErr := f.Close()
			releaseErr := fence.release()
			if releaseErr != nil {
				return nil, fmt.Errorf("%w; fence cleanup: %v", err, releaseErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("%w; close: %v", err, closeErr)
			}
			return nil, err
		}
		fence.files = append(fence.files, f)
	}
	return fence, nil
}
func (f *labelFence) release() error {
	if f == nil || len(f.files) == 0 {
		return nil
	}
	var first error
	for i := len(f.files) - 1; i >= 0; i-- {
		file := f.files[i]
		stale := false
		if _, err := file.Seek(0, 0); err != nil && first == nil {
			first = err
			stale = true
		}
		var raw []byte
		if !stale {
			var err error
			raw, err = io.ReadAll(file)
			if err != nil && first == nil {
				first = err
				stale = true
			}
		}
		var meta labelFenceMeta
		if !stale && (json.Unmarshal(raw, &meta) != nil || meta.Owner != f.owner || meta.Generation != f.generation) {
			stale = true
			if first == nil {
				first = fmt.Errorf("label fence: stale owner release refused")
			}
		}
		if err := file.Sync(); err != nil && first == nil {
			first = err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil && first == nil {
			first = err
		}
		if err := file.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func repairLock(id string) *sync.Mutex {
	v, _ := labelRepairLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// RepairTaskRoleLabel transfers role authority without moving the source row.
// It is idempotent when target already has exactly one owned role label. The
// new label is created for target before it is attached, so a source label ID
// can never be reused for another task.
func RepairTaskRoleLabel(ctx context.Context, p TaskLabelProvider, sourceID, targetID, role string) (err error) {
	return repairTaskRoleLabel(ctx, p, sourceID, targetID, role, LabelRepairOptions{})
}

func repairTaskRoleLabel(ctx context.Context, p TaskLabelProvider, sourceID, targetID, role string, opts LabelRepairOptions) (err error) {
	opts = normalizeRepairOptions(opts, sourceID, targetID)
	if opts.Evidence != nil {
		if err := validateRepairOptions(opts, targetID, role); err != nil {
			return err
		}
	}
	var preSourceLabels, preTargetLabels, postSourceLabels, postTargetLabels []TaskLabel
	var preSourceTask, preTargetTask, postSourceTask, postTargetTask *Task
	var createdForComp TaskLabel
	mutated := false
	defer func() {
		if opts.Evidence == nil {
			return
		}
		outcome, blocked := "success", ""
		if err != nil {
			outcome, blocked = "BLOCKED", err.Error()
		}
		if evidenceErr := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{
			Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project,
			SourceTaskID: sourceID, TargetTaskID: targetID,
			PreSourceLabels: encodeLabelIDs(preSourceLabels), PostSourceLabels: encodeLabelIDs(postSourceLabels),
			PreTargetLabels: encodeLabelIDs(preTargetLabels), PostTargetLabels: encodeLabelIDs(postTargetLabels),
			PreSourceSnapshot: encodeTask(preSourceTask), PostSourceSnapshot: encodeTask(postSourceTask), PreTargetSnapshot: encodeTask(preTargetTask), PostTargetSnapshot: encodeTask(postTargetTask),
			CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation,
			Outcome: outcome, BlockedReason: blocked, Phase: "terminal", Revision: opts.Revision, Operation: opts.Operation,
		}); evidenceErr != nil {
			if mutated && len(preSourceLabels) > 0 {
				if fence, ferr := acquireLabelFence(p, sourceID, targetID); ferr == nil {
					compErr := compensateLabel(ctx, p, sourceID, targetID, preSourceLabels, preTargetLabels, preSourceTask, preTargetTask, createdForComp, postTargetLabels, evidenceErr)
					if rerr := fence.release(); rerr != nil {
						compErr = errors.Join(compErr, rerr)
					}
					if compErr != nil {
						evidenceErr = compErr
					}
				} else {
					evidenceErr = errors.Join(evidenceErr, ferr)
				}
			}
			err = &LabelTransactionError{Cause: err, Compensation: fmt.Errorf("durable label evidence: %w", evidenceErr)}
		}
	}()
	if p == nil || strings.TrimSpace(sourceID) == "" || strings.TrimSpace(targetID) == "" || sourceID == targetID {
		return fmt.Errorf("label repair: invalid provider or task ids")
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return fmt.Errorf("label repair: role required")
	}
	if opts.Evidence != nil {
		if err := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project, SourceTaskID: sourceID, TargetTaskID: targetID, CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation, Outcome: "INTENT", Phase: "intent", Revision: opts.Revision, Operation: opts.Operation}); err != nil {
			return err
		}
	}
	lock := repairLock(targetID)
	lock.Lock()
	defer lock.Unlock()
	fence, err := acquireLabelFence(p, sourceID, targetID)
	if err != nil {
		return &LabelTransactionError{Cause: err, Compensation: ErrLabelTransactionBlocked}
	}
	defer func() {
		if releaseErr := fence.release(); releaseErr != nil {
			err = &LabelTransactionError{Cause: err, Compensation: releaseErr}
		}
	}()

	source, target, err := readLabelState(ctx, p, sourceID, targetID)
	preSourceLabels, preTargetLabels = source, target
	if err != nil {
		return err
	}
	if opts.Evidence != nil {
		observedGeneration, err := ObservedLabelGeneration(ctx, p, sourceID, targetID)
		if err != nil || observedGeneration != opts.Generation {
			return fmt.Errorf("%w: source/target pre-state", ErrLabelGenerationMismatch)
		}
	}
	if err := validateOwned(source, sourceID); err != nil {
		return err
	}
	if err := validateOwned(target, targetID); err != nil {
		return err
	}
	if countRole(source, role) != 1 {
		return fmt.Errorf("label repair: source must have exactly one role label")
	}
	beforeSource, beforeTarget, err := readTaskState(ctx, p, sourceID, targetID)
	preSourceTask, preTargetTask = beforeSource, beforeTarget
	if err != nil {
		return err
	}
	roleRows := make([]TaskLabel, 0)
	canonicalRows := make([]TaskLabel, 0)
	for _, label := range target {
		if !isRoleLabel(label.Name) {
			continue
		}
		roleRows = append(roleRows, label)
		if canonicalRoleLabel(label.Name) == canonicalRoleLabel(role) {
			canonicalRows = append(canonicalRows, label)
		}
	}
	sort.Slice(roleRows, func(i, j int) bool { return roleRows[i].ID < roleRows[j].ID })
	sort.Slice(canonicalRows, func(i, j int) bool { return canonicalRows[i].ID < canonicalRows[j].ID })
	if len(roleRows) == 1 && len(canonicalRows) == 1 {
		postSourceLabels, postTargetLabels = append([]TaskLabel(nil), source...), append([]TaskLabel(nil), target...)
		postSourceTask, postTargetTask = beforeSource, beforeTarget
		return nil
	}

	created := TaskLabel{}
	keepID := ""
	if len(canonicalRows) > 0 {
		keepID = canonicalRows[0].ID
	} else {
		created, err = p.CreateTaskLabel(ctx, targetID, role)
		if err != nil {
			return fmt.Errorf("label repair: create target label: %w", err)
		}
		if created.ID == "" || (created.TaskID != "" && created.TaskID != targetID) {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, TaskLabel{}, nil, fmt.Errorf("created label has unknown or wrong ownership"))
		}
		if opts.Evidence != nil {
			if err := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project, TargetTaskID: targetID, PreTargetLabels: encodeLabelIDs(target), CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation, Outcome: "CREATED", Phase: "created", Revision: opts.Revision, Operation: opts.Operation, CreatedLabelID: created.ID}); err != nil {
				return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, TaskLabel{}, nil, err)
			}
		}
		if proof, ok := p.(LabelCreationProof); !ok {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, TaskLabel{}, nil, fmt.Errorf("created label generation and transaction ownership unproven"))
		} else if err := proof.ProveLabelCreation(ctx, created, targetID, role, opts); err != nil {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, TaskLabel{}, nil, err)
		}
		// Prove the row is unattached before adopting it as this transaction's
		// own. A foreign live row is reported with an empty created identity so
		// no compensation path detaches another card's label.
		if err := assertDonorUnattached(ctx, p, created, targetID); err != nil {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, TaskLabel{}, nil, err)
		}
		createdForComp = created
		if err := p.AttachTaskLabel(ctx, targetID, created.ID); err != nil {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, created, nil, err)
		}
		mutated = true
		keepID = created.ID
	}
	for _, old := range roleRows {
		if old.ID == keepID {
			continue
		}
		if err := p.DetachTaskLabel(ctx, old.ID); err != nil {
			return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, created, nil, err)
		}
		mutated = true
	}

	postSource, postTarget, readErr := readLabelState(ctx, p, sourceID, targetID)
	postSourceLabels, postTargetLabels = postSource, postTarget
	afterSource, afterTarget, taskReadErr := readTaskState(ctx, p, sourceID, targetID)
	postSourceTask, postTargetTask = afterSource, afterTarget
	if readErr == nil && taskReadErr == nil && sameLabels(source, postSource) &&
		sameTask(beforeSource, afterSource) && roleFamilyDelta(beforeTarget, afterTarget, role) &&
		countRole(postTarget, role) == 1 && ownsLabel(postTarget, keepID, targetID) {
		return nil
	}
	cause := readErr
	if cause == nil {
		cause = taskReadErr
	}
	if cause == nil {
		cause = fmt.Errorf("source or target label readback mismatch")
	}
	return compensateLabel(ctx, p, sourceID, targetID, source, target, beforeSource, beforeTarget, created, postTarget, cause)
}

// EnsureTaskRoleLabel repairs a label-less target without inventing a source
// task. It is the safe path for orphan/zero-label rows; an empty role remains
// blocked because no intended authority exists.
func EnsureTaskRoleLabel(ctx context.Context, p TaskLabelProvider, targetID, role string) (err error) {
	return ensureTaskRoleLabel(ctx, p, targetID, role, LabelRepairOptions{})
}

// RepairTaskRoleLabelWithOptions is the production entrypoint. The legacy
// wrapper remains for adapters and focused unit tests that do not persist
// evidence.
func RepairTaskRoleLabelWithOptions(ctx context.Context, p TaskLabelProvider, sourceID, targetID, role string, opts LabelRepairOptions) error {
	if opts.Evidence == nil {
		return fmt.Errorf("label repair: durable evidence authority required")
	}
	return repairTaskRoleLabel(ctx, p, sourceID, targetID, role, opts)
}

func EnsureTaskRoleLabelWithOptions(ctx context.Context, p TaskLabelProvider, targetID, role string, opts LabelRepairOptions) error {
	if opts.Evidence == nil {
		return fmt.Errorf("label ensure: durable evidence authority required")
	}
	return ensureTaskRoleLabel(ctx, p, targetID, role, opts)
}

func normalizeRepairOptions(opts LabelRepairOptions, sourceID, targetID string) LabelRepairOptions {
	if opts.TransactionID == "" {
		opts.TransactionID = fmt.Sprintf("label-repair:%s:%s:%s:%s", sourceID, targetID, opts.Revision, opts.Operation)
	}
	if opts.Generation == "" {
		opts.Generation = opts.Revision
	}
	if opts.Operation == "" {
		opts.Operation = "role-label"
	}
	return opts
}

func validateRepairOptions(opts LabelRepairOptions, targetID, role string) error {
	for name, value := range map[string]string{"repository": opts.Repository, "provider": opts.Provider, "project": opts.Project, "target": targetID, "role": role, "revision": opts.Revision, "generation": opts.Generation} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("label evidence: %s identity required", name)
		}
	}
	if opts.Revision != opts.Generation {
		return fmt.Errorf("label evidence: revision/generation mismatch")
	}
	return nil
}

func encodeLabelIDs(labels []TaskLabel) string {
	ids := make([]string, 0, len(labels))
	for _, label := range labels {
		ids = append(ids, label.ID)
	}
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	return string(b)
}

func encodeTask(task *Task) string {
	if task == nil {
		return ""
	}
	b, _ := json.Marshal(task)
	return string(b)
}

// ObservedLabelGeneration is a deterministic digest of complete task and
// owned-label readback. Callers may pass it back as the generation authority;
// arbitrary caller strings are rejected by the transaction path.
func ObservedLabelGeneration(ctx context.Context, p TaskLabelProvider, ids ...string) (string, error) {
	if p == nil || len(ids) == 0 {
		return "", fmt.Errorf("label generation: provider and task identities required")
	}
	type snapshot struct {
		Task   *Task
		Labels []TaskLabel
	}
	snapshots := make([]snapshot, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("label generation: task identity required")
		}
		task, err := p.GetTask(ctx, id)
		if err != nil || task == nil {
			return "", fmt.Errorf("label generation: task %s: %w", id, err)
		}
		labels, err := p.ListTaskLabels(ctx, id)
		if err != nil {
			return "", err
		}
		if err := validateOwned(labels, id); err != nil {
			return "", err
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
		snapshots = append(snapshots, snapshot{Task: task, Labels: labels})
	}
	b, _ := json.Marshal(snapshots)
	digest := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func ensureTaskRoleLabel(ctx context.Context, p TaskLabelProvider, targetID, role string, opts LabelRepairOptions) (err error) {
	opts = normalizeRepairOptions(opts, "", targetID)
	if opts.Evidence != nil {
		if err := validateRepairOptions(opts, targetID, role); err != nil {
			return err
		}
	}
	var before, after []TaskLabel
	var beforeTask, afterTask *Task
	var createdForComp TaskLabel
	mutated := false
	defer func() {
		if opts.Evidence == nil {
			return
		}
		outcome, blocked := "success", ""
		if err != nil {
			outcome, blocked = "BLOCKED", err.Error()
		}
		if evidenceErr := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{
			Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project,
			TargetTaskID: targetID, PreTargetLabels: encodeLabelIDs(before), PostTargetLabels: encodeLabelIDs(after),
			PreTargetSnapshot: encodeTask(beforeTask), PostTargetSnapshot: encodeTask(afterTask),
			CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation,
			Outcome: outcome, BlockedReason: blocked, Phase: "terminal", Revision: opts.Revision, Operation: opts.Operation,
		}); evidenceErr != nil {
			if mutated && beforeTask != nil {
				if fence, ferr := acquireLabelFence(p, "", targetID); ferr == nil {
					compErr := compensateTargetLabel(ctx, p, targetID, before, beforeTask, createdForComp, evidenceErr)
					if rerr := fence.release(); rerr != nil {
						compErr = errors.Join(compErr, rerr)
					}
					if compErr != nil {
						evidenceErr = compErr
					}
				} else {
					evidenceErr = errors.Join(evidenceErr, ferr)
				}
			}
			err = &LabelTransactionError{Cause: err, Compensation: fmt.Errorf("durable label evidence: %w", evidenceErr)}
		}
	}()
	if p == nil || strings.TrimSpace(targetID) == "" || strings.TrimSpace(role) == "" {
		return fmt.Errorf("label ensure: target and intended role required")
	}
	if opts.Evidence != nil {
		if err := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project, TargetTaskID: targetID, CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation, Outcome: "INTENT", Phase: "intent", Revision: opts.Revision, Operation: opts.Operation}); err != nil {
			return err
		}
	}
	fence, err := acquireLabelFence(p, "", targetID)
	if err != nil {
		return &LabelTransactionError{Cause: err, Compensation: ErrLabelTransactionBlocked}
	}
	defer func() {
		if releaseErr := fence.release(); releaseErr != nil {
			err = &LabelTransactionError{Cause: err, Compensation: releaseErr}
		}
	}()
	before, err = p.ListTaskLabels(ctx, targetID)
	if err != nil {
		return err
	}
	if err := validateOwned(before, targetID); err != nil {
		return err
	}
	if opts.Evidence != nil {
		observedGeneration, err := ObservedLabelGeneration(ctx, p, targetID)
		if err != nil || observedGeneration != opts.Generation {
			return fmt.Errorf("%w: target pre-state", ErrLabelGenerationMismatch)
		}
	}
	roleRows := make([]TaskLabel, 0)
	for _, label := range before {
		if isRoleLabel(label.Name) {
			roleRows = append(roleRows, label)
		}
	}
	sort.Slice(roleRows, func(i, j int) bool { return roleRows[i].ID < roleRows[j].ID })
	canonicalRows := make([]TaskLabel, 0, len(roleRows))
	for _, label := range roleRows {
		if canonicalRoleLabel(label.Name) == canonicalRoleLabel(role) {
			canonicalRows = append(canonicalRows, label)
		}
	}
	var ignoredTask *Task
	beforeTask, ignoredTask, err = readTaskState(ctx, p, targetID, targetID)
	_ = ignoredTask
	if err != nil {
		return err
	}
	if len(roleRows) == 1 && len(canonicalRows) == 1 {
		after = append([]TaskLabel(nil), before...)
		afterTask = beforeTask
		return nil
	}
	created := TaskLabel{}
	if len(canonicalRows) == 0 {
		created, err = p.CreateTaskLabel(ctx, targetID, role)
		if err != nil {
			return err
		}
		if created.ID == "" || (created.TaskID != "" && created.TaskID != targetID) {
			return &LabelTransactionError{Cause: ErrLabelOwnershipUnknown, Compensation: ErrLabelTransactionBlocked}
		}
		if opts.Evidence != nil {
			if err := opts.Evidence.RecordLabelRepairEvidence(LabelRepairEvidence{Repository: opts.Repository, Provider: opts.Provider, Project: opts.Project, TargetTaskID: targetID, PreTargetLabels: encodeLabelIDs(before), CanonicalRole: role, TransactionID: opts.TransactionID, Generation: opts.Generation, Outcome: "CREATED", Phase: "created", Revision: opts.Revision, Operation: opts.Operation, CreatedLabelID: created.ID}); err != nil {
				return compensateTargetLabel(ctx, p, targetID, before, beforeTask, TaskLabel{}, err)
			}
		}
		if proof, ok := p.(LabelCreationProof); !ok {
			return compensateTargetLabel(ctx, p, targetID, before, beforeTask, TaskLabel{}, fmt.Errorf("created label generation and transaction ownership unproven"))
		} else if err := proof.ProveLabelCreation(ctx, created, targetID, role, opts); err != nil {
			return compensateTargetLabel(ctx, p, targetID, before, beforeTask, TaskLabel{}, err)
		}
		// Same donor proof as the repair path: an attached row is somebody
		// else's, so it is never adopted and never compensated against.
		if err := assertDonorUnattached(ctx, p, created, targetID); err != nil {
			return compensateTargetLabel(ctx, p, targetID, before, beforeTask, TaskLabel{}, err)
		}
		createdForComp = created
		if err := p.AttachTaskLabel(ctx, targetID, created.ID); err != nil {
			return compensateTargetLabel(ctx, p, targetID, before, beforeTask, created, err)
		}
		mutated = true
		roleRows = append([]TaskLabel{created}, roleRows...)
	}
	if len(canonicalRows) > 0 {
		keep := canonicalRows[0]
		rest := make([]TaskLabel, 0, len(roleRows))
		for _, row := range roleRows {
			if row.ID != keep.ID {
				rest = append(rest, row)
			}
		}
		roleRows = append([]TaskLabel{keep}, rest...)
	}
	// Keep one deterministic role-family row and detach every other role row.
	for _, old := range roleRows[1:] {
		if err := p.DetachTaskLabel(ctx, old.ID); err != nil {
			return compensateTargetLabel(ctx, p, targetID, before, beforeTask, created, err)
		}
		mutated = true
	}
	after, readErr := p.ListTaskLabels(ctx, targetID)
	// Keep the post-snapshot available to the durable evidence defer even when
	// exact-delta validation rejects it.
	_ = after
	afterTask, _, taskErr := readTaskState(ctx, p, targetID, targetID)
	// afterTask is retained for terminal snapshot evidence.
	keptID := roleRows[0].ID
	if readErr == nil && taskErr == nil && countRole(after, role) == 1 && ownsLabel(after, keptID, targetID) && roleFamilyDelta(beforeTask, afterTask, role) {
		return nil
	}
	if readErr == nil {
		readErr = taskErr
	}
	if readErr == nil {
		readErr = fmt.Errorf("target label readback mismatch")
	}
	return compensateTargetLabel(ctx, p, targetID, before, beforeTask, created, readErr)
}

// compensateTargetLabel undoes a label this transaction attached. It detaches
// only: a workspace-label delete resolves by name and would take every row
// sharing this row's name with it, and rollback fires exactly when a
// reconciliation is already failing. The row is left unattached instead.
func compensateTargetLabel(ctx context.Context, p TaskLabelProvider, targetID string, original []TaskLabel, originalTask *Task, created TaskLabel, cause error) error {
	var comp error
	if created.ID != "" {
		if err := p.DetachTaskLabel(ctx, created.ID); err != nil {
			comp = err
		}
	}
	// Restore any pre-existing, provider-owned rows detached by a family
	// reconciliation. Never use the untrusted create response for this step.
	if current, err := p.ListTaskLabels(ctx, targetID); err == nil {
		for _, wanted := range original {
			if ownsLabel(current, wanted.ID, targetID) {
				continue
			}
			if err := p.AttachTaskLabel(ctx, targetID, wanted.ID); err != nil && comp == nil {
				comp = err
			}
		}
	} else if comp == nil {
		comp = err
	}
	got, err := p.ListTaskLabels(ctx, targetID)
	if err != nil && comp == nil {
		comp = err
	}
	gotTask, _, terr := readTaskState(ctx, p, targetID, targetID)
	if terr != nil && comp == nil {
		comp = terr
	}
	if comp == nil && (!sameLabels(original, got) || !sameTask(originalTask, gotTask)) {
		comp = fmt.Errorf("compensation readback mismatch")
	}
	if comp != nil {
		return &LabelTransactionError{Cause: cause, Compensation: comp}
	}
	return cause
}

func readLabelState(ctx context.Context, p TaskLabelProvider, sourceID, targetID string) ([]TaskLabel, []TaskLabel, error) {
	source, err := p.ListTaskLabels(ctx, sourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("label repair: read source: %w", err)
	}
	target, err := p.ListTaskLabels(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("label repair: read target: %w", err)
	}
	return source, target, nil
}

func readTaskState(ctx context.Context, p TaskLabelProvider, sourceID, targetID string) (*Task, *Task, error) {
	source, err := p.GetTask(ctx, sourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("label repair: read source task: %w", err)
	}
	target, err := p.GetTask(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("label repair: read target task: %w", err)
	}
	if source == nil || target == nil {
		return nil, nil, fmt.Errorf("label repair: nil task snapshot")
	}
	return source, target, nil
}

func sameTask(a, b *Task) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	return ac.ID == bc.ID && ac.Ref == bc.Ref && ac.Title == bc.Title && ac.Description == bc.Description &&
		ac.Status == bc.Status && ac.Priority == bc.Priority && ac.ProjectID == bc.ProjectID && ac.CreatedAt.Equal(bc.CreatedAt) &&
		sameStrings(ac.Labels, bc.Labels)
}

func targetRoleDelta(before, after *Task, role string) bool {
	if before == nil || after == nil || before.ID != after.ID || before.Ref != after.Ref || before.Title != after.Title || before.Description != after.Description || before.Status != after.Status || before.Priority != after.Priority || before.ProjectID != after.ProjectID || !before.CreatedAt.Equal(after.CreatedAt) {
		return false
	}
	a, b := append([]string(nil), before.Labels...), append([]string(nil), after.Labels...)
	sort.Strings(a)
	sort.Strings(b)
	removed, added := []string{}, []string{}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i == len(a):
			added = append(added, b[j])
			j++
		case j == len(b):
			removed = append(removed, a[i])
			i++
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			removed = append(removed, a[i])
			i++
		default:
			added = append(added, b[j])
			j++
		}
	}
	return len(removed) == 0 && len(added) == 1 && strings.EqualFold(added[0], role)
}

func roleFamilyDelta(before, after *Task, role string) bool {
	if before == nil || after == nil || before.ID != after.ID || before.Ref != after.Ref || before.Title != after.Title || before.Description != after.Description || before.Status != after.Status || before.Priority != after.Priority || before.ProjectID != after.ProjectID || !before.CreatedAt.Equal(after.CreatedAt) {
		return false
	}
	filter := func(labels []string) []string {
		out := make([]string, 0, len(labels)+1)
		for _, label := range labels {
			if !isRoleLabel(label) {
				out = append(out, label)
			}
		}
		out = append(out, role)
		sort.Strings(out)
		return out
	}
	a := make([]string, 0, len(after.Labels))
	for _, label := range after.Labels {
		if isRoleLabel(label) {
			a = append(a, canonicalRoleLabel(label))
		} else {
			a = append(a, label)
		}
	}
	b := filter(before.Labels)
	sort.Strings(a)
	return sameStrings(a, b)
}

func isRoleLabel(name string) bool {
	switch canonicalRoleLabel(name) {
	case "worker", "forge-smith", "herd-smith", "smith", "reviewer", "scout-planner", "orchestrator", "verification-gate", "review-supervisor", "harvest", "recovery-sentinel":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "role:")
	}
}

func canonicalRoleLabel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(name, "role:") {
		name = strings.TrimSpace(strings.TrimPrefix(name, "role:"))
	}
	switch name {
	case "herd-smith", "smith", "forge-smith":
		return "forge-smith"
	default:
		return name
	}
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateOwned(labels []TaskLabel, taskID string) error {
	for _, l := range labels {
		if l.ID == "" || l.TaskID == "" {
			return ErrLabelOwnershipUnknown
		}
		if l.TaskID != taskID {
			return fmt.Errorf("label %q owned by %q, not %q", l.ID, l.TaskID, taskID)
		}
	}
	return nil
}

func countRole(labels []TaskLabel, role string) int {
	n := 0
	for _, l := range labels {
		if canonicalRoleLabel(l.Name) == canonicalRoleLabel(role) {
			n++
		}
	}
	return n
}
func ownsLabel(labels []TaskLabel, id, taskID string) bool {
	for _, l := range labels {
		if l.ID == id && l.TaskID == taskID {
			return true
		}
	}
	return false
}
func sameLabels(a, b []TaskLabel) bool {
	ca, cb := append([]TaskLabel(nil), a...), append([]TaskLabel(nil), b...)
	sort.Slice(ca, func(i, j int) bool { return ca[i].ID < ca[j].ID })
	sort.Slice(cb, func(i, j int) bool { return cb[i].ID < cb[j].ID })
	if len(ca) != len(cb) {
		return false
	}
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

// compensateLabel detaches only, for the same reason as compensateTargetLabel:
// the row this transaction created is left orphaned rather than risking a
// name-wide workspace delete on a failing rollback path.
func compensateLabel(ctx context.Context, p TaskLabelProvider, sourceID, targetID string, source, originalTarget []TaskLabel, originalSourceTask, originalTargetTask *Task, created TaskLabel, target []TaskLabel, cause error) error {
	var comp error
	if created.ID != "" {
		if err := p.DetachTaskLabel(ctx, created.ID); err != nil {
			comp = err
		}
	}
	if current, err := p.ListTaskLabels(ctx, targetID); err == nil {
		for _, wanted := range originalTarget {
			if ownsLabel(current, wanted.ID, targetID) {
				continue
			}
			if err := p.AttachTaskLabel(ctx, targetID, wanted.ID); err != nil && comp == nil {
				comp = err
			}
		}
	} else if comp == nil {
		comp = err
	}
	if gotSource, gotTarget, err := readLabelState(ctx, p, sourceID, targetID); err != nil && comp == nil {
		comp = err
	} else if comp == nil && (!sameLabels(source, gotSource) || !sameLabels(originalTarget, gotTarget)) {
		comp = fmt.Errorf("compensation readback mismatch")
	}
	if gotSourceTask, gotTargetTask, err := readTaskState(ctx, p, sourceID, targetID); err != nil && comp == nil {
		comp = err
	} else if comp == nil && (!sameTask(originalSourceTask, gotSourceTask) || !sameTask(originalTargetTask, gotTargetTask)) {
		comp = fmt.Errorf("compensation task readback mismatch")
	}
	if comp != nil {
		return &LabelTransactionError{Cause: cause, Compensation: comp}
	}
	return cause
}
