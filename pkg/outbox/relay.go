package outbox

import (
	"context"
	"errors"
	"time"
)

// Handler delivers outbox items of one Kind to their external system
// (provider board, Herdr, Git). Implementations SHOULD be idempotent:
// Relay claims an item exclusively before calling Send (so two Relay
// instances never both dispatch the same item), but a Relay process that
// crashes between Send returning and MarkSent committing leaves the item
// in_flight forever unless something reconciles it back to pending —
// that reconciliation is out of this package's scope (see
// lifecycle.Reconciler for the analogous state-machine sweep); a Handler
// that can safely run twice is the cheapest mitigation available today.
//
// FAC-121 (Herdr dispatch) and future provider/Git side effects register
// their own Handler here rather than the lifecycle/outbox packages
// growing knowledge of worktrees, Herdr sessions, or provider APIs.
type Handler interface {
	Kind() string
	Send(ctx context.Context, item Item) error
}

const defaultMaxAttempts = 8

// Relay delivers pending, due outbox items to their registered Handler.
type Relay struct {
	store       *Store
	handlers    map[string]Handler
	MaxAttempts int
	BatchSize   int
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
}

// NewRelay builds a Relay backed by store, dispatching to handlers keyed
// by their Kind().
func NewRelay(store *Store, handlers ...Handler) *Relay {
	r := &Relay{
		store:       store,
		handlers:    make(map[string]Handler),
		MaxAttempts: defaultMaxAttempts,
		BatchSize:   50,
		Now:         time.Now,
	}
	for _, h := range handlers {
		r.RegisterHandler(h)
	}
	return r
}

// RegisterHandler adds (or replaces) the handler for its Kind.
func (r *Relay) RegisterHandler(h Handler) {
	r.handlers[h.Kind()] = h
}

// RelayOnce delivers one batch of pending, due items and returns how many
// were successfully sent. Items whose kind has no registered handler are
// left pending — ponytail: no dead-letter queue yet, add one if an
// unregistered kind needs operator visibility beyond the pending count.
//
// Each item is Claimed (pending -> in_flight, exclusive) before Send is
// called. If the claim loses the race — another Relay instance, in this
// process or another, already took it — this Relay just skips it rather
// than erroring the whole pass.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	items, err := r.store.Pending("", r.BatchSize, r.Now())
	if err != nil {
		return 0, err
	}

	sent := 0
	for _, candidate := range items {
		handler, ok := r.handlers[candidate.Kind]
		if !ok {
			continue
		}

		claimed, err := r.store.Claim(candidate.ID)
		if err != nil {
			if errors.Is(err, ErrNotClaimable) {
				continue
			}
			return sent, err
		}

		if err := handler.Send(ctx, claimed); err != nil {
			if markErr := r.store.MarkFailed(claimed.ID, err.Error(), r.MaxAttempts); markErr != nil {
				return sent, markErr
			}
			continue
		}
		if err := r.store.MarkSent(claimed.ID); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
