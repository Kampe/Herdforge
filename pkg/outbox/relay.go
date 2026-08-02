package outbox

import "context"

// Handler delivers outbox items of one Kind to their external system
// (provider board, Herdr, Git). Implementations MUST be idempotent: Relay
// may call Send more than once for the same item if a prior attempt's
// success was never durably recorded (e.g. Relay crashed between Send
// returning and MarkSent committing).
//
// FAC-121 (Herdr dispatch) and future provider/Git side effects register
// their own Handler here rather than the lifecycle/outbox packages
// growing knowledge of worktrees, Herdr sessions, or provider APIs.
type Handler interface {
	Kind() string
	Send(ctx context.Context, item Item) error
}

const defaultMaxAttempts = 8

// Relay delivers pending outbox items to their registered Handler.
type Relay struct {
	store       *Store
	handlers    map[string]Handler
	MaxAttempts int
	BatchSize   int
}

// NewRelay builds a Relay backed by store, dispatching to handlers keyed
// by their Kind().
func NewRelay(store *Store, handlers ...Handler) *Relay {
	r := &Relay{
		store:       store,
		handlers:    make(map[string]Handler),
		MaxAttempts: defaultMaxAttempts,
		BatchSize:   50,
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

// RelayOnce delivers one batch of pending items and returns how many were
// successfully sent. Items whose kind has no registered handler are left
// pending — ponytail: no dead-letter queue yet, add one if an
// unregistered kind needs operator visibility beyond the pending count.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	items, err := r.store.Pending("", r.BatchSize)
	if err != nil {
		return 0, err
	}

	sent := 0
	for _, item := range items {
		handler, ok := r.handlers[item.Kind]
		if !ok {
			continue
		}
		if err := handler.Send(ctx, item); err != nil {
			if markErr := r.store.MarkFailed(item.ID, err.Error(), r.MaxAttempts); markErr != nil {
				return sent, markErr
			}
			continue
		}
		if err := r.store.MarkSent(item.ID); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
