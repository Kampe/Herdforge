package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// FencedCAS is the local half of FAC-147 fencing. Authoritative acceptance
// of fence+op MUST occur inside mutate (Kaneo HTTP with X-Herd-* headers
// or an equivalent broker). Local receipts are a cache for reconcile; the
// provider-side applied set is the crash-safe source of truth.
type FencedCAS struct {
	fences FenceStore
	reader TaskProvider
}

func NewFencedCAS(fences FenceStore, reader TaskProvider) (*FencedCAS, error) {
	if fences == nil {
		return nil, fmt.Errorf("provider: NewFencedCAS requires a FenceStore")
	}
	if reader == nil {
		return nil, fmt.Errorf("provider: NewFencedCAS requires a TaskProvider reader")
	}
	return &FencedCAS{fences: fences, reader: reader}, nil
}

func OpenFencedCAS(path string, reader TaskProvider) (*FencedCAS, error) {
	store, err := NewSQLiteFenceStore(path)
	if err != nil {
		return nil, err
	}
	cas, err := NewFencedCAS(store, reader)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return cas, nil
}

// canAttributeEmptyRevOnStore: empty post-remote revision + Present + live
// advanced is NEVER attributable from local receipts alone — a direct client
// or other host can advance the board with no local applied op (audit).
// Returns false always for live!=base; caller re-mutates only when live==base.
func canAttributeEmptyRevOnStore(ctx context.Context, store FenceStore, taskID, opID, liveRev, baseRev string) (bool, error) {
	_ = ctx
	_ = store
	_ = taskID
	_ = opID
	if liveRev == "" {
		return false, fmt.Errorf("%w: empty live revision cannot attribute", claim.ErrProviderAmbiguous)
	}
	// live==base means no external advance observed — not an attribute case.
	if baseRev != "" && liveRev == baseRev {
		return false, nil
	}
	// live advanced without durable post-remote revision on THIS op: refuse.
	// Causal recovery requires stored post-remote Revision (crash after persist).
	return false, nil
}

func (c *FencedCAS) Close() error {
	if c == nil || c.fences == nil {
		return nil
	}
	return c.fences.Close()
}

func EncodeRevision(t *Task) claim.ProviderRevision {
	if t == nil {
		return ""
	}
	status := NormalizeStatus(t.Status)
	if !t.UpdatedAt.IsZero() {
		return claim.ProviderRevision(fmt.Sprintf("%s|%s|%s",
			status, t.ID, t.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	}
	created := ""
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return claim.ProviderRevision(fmt.Sprintf("%s|%s|%s", status, t.ID, created))
}

func (c *FencedCAS) ReadRevision(ctx context.Context, taskID string) (claim.ProviderRevision, error) {
	if c == nil || c.reader == nil {
		return "", fmt.Errorf("provider: FencedCAS has no reader")
	}
	t, err := c.reader.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("provider: read revision for %s: %w", taskID, err)
	}
	return EncodeRevision(t), nil
}

func (c *FencedCAS) CompareAndSwap(
	ctx context.Context,
	taskID string,
	expected claim.ProviderRevision,
	fenceToken int64,
	opID string,
	mutate func(ctx context.Context) error,
) (claim.ProviderRevision, error) {
	if c == nil {
		return "", fmt.Errorf("provider: nil FencedCAS")
	}
	if mutate == nil {
		return "", fmt.Errorf("provider: CompareAndSwap requires a mutate func")
	}
	if opID == "" {
		return "", fmt.Errorf("provider: CompareAndSwap requires opID")
	}

	expStatus := casExpectedStatus(ctx)
	expComment := casExpectedComment(ctx)

	var outRev claim.ProviderRevision
	var outErr error
	exErr := c.fences.WithExclusive(ctx, taskID, func(ctx context.Context) error {
		// 1. High-water BEFORE any ambiguous Present/Absent reconcile
		// (hold inb04ouq): gen1 must not MarkApplied after gen2 merely
		// because board effects are equivalent.
		high, err := c.fences.Highest(ctx, taskID)
		if err != nil {
			outErr = err
			return err
		}
		if fenceToken < high {
			cur, rerr := c.ReadRevision(ctx, taskID)
			if rerr != nil {
				outErr = fmt.Errorf("%w: read revision after fence reject: %v", claim.ErrProviderFenceRejected, rerr)
				return outErr
			}
			outRev = cur
			outErr = fmt.Errorf("%w: fence token %d is behind %d for %s",
				claim.ErrProviderFenceRejected, fenceToken, high, taskID)
			return outErr
		}

		// 2. Local receipt cache (only after fence still current).
		if rec, err := c.fences.LookupApplied(ctx, opID); err != nil {
			outErr = err
			return err
		} else if rec != nil {
			if rec.TaskID != taskID || rec.FenceToken != fenceToken {
				outErr = fmt.Errorf("%w: op %s bound to task=%s fence=%d, request task=%s fence=%d",
					claim.ErrProviderFenceRejected, opID, rec.TaskID, rec.FenceToken, taskID, fenceToken)
				return outErr
			}
			if !rec.Ambiguous {
				outRev = claim.ProviderRevision(rec.Revision)
				if outRev == "" {
					live, rerr := c.ReadRevision(ctx, taskID)
					if rerr != nil {
						outErr = fmt.Errorf("%w: applied receipt missing revision and live read failed: %v",
							claim.ErrProviderAmbiguous, rerr)
						return outErr
					}
					outRev = live
				}
				return nil
			}
			// Ambiguous under a still-current fence: live effect only.
			// Status ops require durable revision evidence matching live
			// (never invent revision after the fact — hold vzai0t1e #5).
			ok, verr := c.verifyExpectation(ctx, taskID, rec, true /*strict*/)
			if verr != nil {
				outErr = fmt.Errorf("%w: %v", claim.ErrProviderAmbiguous, verr)
				return outErr
			}
			if ok {
				settle := true
				if rec.ExpectedStatus != "" && rec.ExpectedComment == "" {
					live, lerr := c.ReadRevision(ctx, taskID)
					if lerr != nil || live == "" {
						outErr = fmt.Errorf("%w: ambiguous status op %s live revision unavailable: %v",
							claim.ErrProviderAmbiguous, opID, lerr)
						return outErr
					}
					if rec.Revision == "" {
						if rec.BaseRevision != "" && string(live) == rec.BaseRevision {
							settle = false // re-mutate
						} else {
							// Attribute empty-rev only without competing owner of live.
							okAttr, aerr := canAttributeEmptyRevOnStore(ctx, c.fences, taskID, opID, string(live), rec.BaseRevision)
							if aerr != nil {
								outErr = aerr
								return outErr
							}
							if !okAttr {
								outErr = fmt.Errorf("%w: refuse empty-rev Present settle for op %s — competing same-status (base=%q live=%q)",
									claim.ErrProviderAmbiguous, opID, rec.BaseRevision, live)
								return outErr
							}
							rec.Revision = string(live)
							outRev = live
						}
					} else if string(live) != rec.Revision {
						outErr = fmt.Errorf("%w: ambiguous status op %s revision mismatch (stored=%q live=%q)",
							claim.ErrProviderAmbiguous, opID, rec.Revision, live)
						return outErr
					} else {
						outRev = live
					}
				} else {
					// Comment / other recovery: live revision is authoritative evidence.
					live, rerr := c.ReadRevision(ctx, taskID)
					if rerr != nil {
						outErr = fmt.Errorf("%w: comment recovery ReadRevision: %v", claim.ErrProviderAmbiguous, rerr)
						return outErr
					}
					if rec.Revision == "" && live != "" {
						rec.Revision = string(live)
					}
					outRev = claim.ProviderRevision(rec.Revision)
					if outRev == "" {
						outRev = live
					}
				}
				if settle {
					if err := c.fences.MarkApplied(ctx, *rec); err != nil {
						outErr = err
						return err
					}
					return nil
				}
			}
			// ABSENT under current fence: fall through to re-mutate.
		}

		// 3. Advance high-water if this fence is ahead (new generation).
		if fenceToken > high {
			if _, err := c.fences.Advance(ctx, taskID, fenceToken); err != nil {
				outErr = err
				return err
			}
		}

		cur, err := c.ReadRevision(ctx, taskID)
		if err != nil {
			outErr = err
			return err
		}
		if cur != expected {
			outRev = cur
			outErr = fmt.Errorf("%w: expected %s, current %s",
				claim.ErrProviderRevisionStale, expected, cur)
			return outErr
		}

		// 3. Authoritative mutate: inject fence+op into context for Kaneo HTTP.
		// Mark exclusive held so AuthBroker does not re-lock the same task.
		mctx := WithCASMeta(ctx, fenceToken, opID)
		mctx = WithCASExpectation(mctx, expStatus, expComment)
		mctx = withExclusiveHeld(mctx, taskID)
		if err := mutate(mctx); err != nil {
			outRev = cur
			outErr = err
			return err
		}

		// 4. Provider accepted the mutation (including server-side op dedupe).
		// Persist local receipt — errors are fatal, not ignored.
		newRev, rerr := c.ReadRevision(ctx, taskID)
		rec := OpReceipt{
			OpID:            opID,
			TaskID:          taskID,
			FenceToken:      fenceToken,
			ExpectedStatus:  expStatus,
			ExpectedComment: expComment,
		}
		if rerr != nil {
			if err := c.fences.MarkAmbiguous(ctx, rec); err != nil {
				outErr = fmt.Errorf("%w: mark ambiguous after mutate: %v (read: %v)", claim.ErrProviderAmbiguous, err, rerr)
				return outErr
			}
			outErr = fmt.Errorf("%w: post-mutate revision for %s: %v", claim.ErrProviderAmbiguous, taskID, rerr)
			return outErr
		}
		// Verify expectation when provided (status always; comment when listable).
		rec.Revision = string(newRev)
		if ok, verr := c.verifyExpectation(ctx, taskID, &rec, false /*strict*/); verr != nil || !ok {
			if err := c.fences.MarkAmbiguous(ctx, rec); err != nil {
				outErr = err
				return err
			}
			if verr != nil {
				outErr = fmt.Errorf("%w: %v", claim.ErrProviderAmbiguous, verr)
			} else {
				outErr = fmt.Errorf("%w: board does not match expected effect for op %s", claim.ErrProviderAmbiguous, opID)
			}
			return outErr
		}
		if err := c.fences.MarkApplied(ctx, rec); err != nil {
			// Provider already committed; local receipt failure is ambiguous.
			// MarkAmbiguous errors must propagate (never ignored).
			if merr := c.fences.MarkAmbiguous(ctx, rec); merr != nil {
				outErr = fmt.Errorf("%w: MarkApplied: %v; MarkAmbiguous: %v", claim.ErrProviderAmbiguous, err, merr)
			} else {
				outErr = fmt.Errorf("%w: local MarkApplied failed after provider success: %v", claim.ErrProviderAmbiguous, err)
			}
			outRev = newRev
			return outErr
		}
		outRev = newRev
		return nil
	})
	if exErr != nil {
		if outErr != nil {
			return outRev, outErr
		}
		return outRev, exErr
	}
	return outRev, outErr
}

// verifyExpectation checks board effect. strict=true is for ambiguous
// reconcile: missing comment list capability keeps the op ambiguous
// rather than falsely accepting a readable task. strict=false is the
// post-mutate path: provider already returned success (incl. server-side
// op receipt); comment body is verified only when the reader can list.
func (c *FencedCAS) verifyExpectation(ctx context.Context, taskID string, rec *OpReceipt, strict bool) (bool, error) {
	if rec == nil {
		return false, fmt.Errorf("nil receipt")
	}
	t, err := c.reader.GetTask(ctx, taskID)
	if err != nil {
		// Live status UNKNOWN — must not fall through to re-mutate as ABSENT.
		return false, fmt.Errorf("status live UNKNOWN: %w", err)
	}
	if rec.ExpectedStatus != "" && NormalizeStatus(t.Status) != NormalizeStatus(rec.ExpectedStatus) {
		// ABSENT (live read succeeded, effect not present) — nil error so
		// recovery may re-mutate under a still-current fence.
		return false, nil
	}
	if rec.ExpectedComment != "" {
		// Prefer LIVE ListLiveComments (Kaneo). Never substitute local
		// AuthBroker receipts. Match op-bound identity when OpID set.
		opID := rec.OpID
		if lr, ok := c.reader.(interface {
			ListLiveComments(ctx context.Context, taskID string) ([]string, error)
		}); ok {
			live, err := lr.ListLiveComments(ctx, taskID)
			if err != nil {
				return false, fmt.Errorf("comment live UNKNOWN: %w", err)
			}
			for _, cmt := range live {
				// When OpID is set, only op-bound marker match — never bare body.
				if opID != "" {
					if MatchCommentOp(cmt, rec.ExpectedComment, opID) {
						return true, nil
					}
					continue
				}
				if cmt == rec.ExpectedComment {
					return true, nil
				}
			}
			return false, nil // ABSENT
		}
		if cr, ok := c.reader.(interface {
			Comments(taskID string) []string
		}); ok {
			for _, cmt := range cr.Comments(taskID) {
				if opID != "" {
					if MatchCommentOp(cmt, rec.ExpectedComment, opID) {
						return true, nil
					}
					continue
				}
				if cmt == rec.ExpectedComment {
					return true, nil
				}
			}
			return false, nil
		}
		if strict {
			return false, fmt.Errorf("comment %q live UNKNOWN: reader has no list", rec.ExpectedComment)
		}
		return true, nil
	}
	return true, nil
}

func (c *FencedCAS) AdvanceFence(ctx context.Context, taskID string, fenceToken int64) error {
	if c == nil {
		return fmt.Errorf("provider: nil FencedCAS")
	}
	return c.fences.WithExclusive(ctx, taskID, func(ctx context.Context) error {
		_, err := c.fences.Advance(ctx, taskID, fenceToken)
		return err
	})
}

var _ claim.ProviderCAS = (*FencedCAS)(nil)
