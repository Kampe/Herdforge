package backfill

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// LifecycleSource tails the durable lifecycle event log. It is the production
// EventSource: the same rows a transition wrote in its own transaction are
// what wakes the coordinator, so an event that is durable is an event the
// watcher will see — with or without a callback.
type LifecycleSource struct {
	Store *lifecycle.EventStore
}

// Since implements EventSource over lifecycle_events.id.
//
// The lifecycle store's API is not context-aware, so ctx bounds nothing here.
// That is honest rather than convenient: pretending to honour cancellation
// would hide a blocked read behind a timeout that never fires.
func (s LifecycleSource) Since(_ context.Context, after int64, limit int) ([]Event, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("backfill: lifecycle source has no store")
	}
	rows, err := s.Store.EventsSince(after, limit)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(rows))
	for _, ev := range rows {
		events = append(events, Event{
			Sequence: ev.ID,
			Repo:     ev.Repo,
			TaskRef:  ev.TaskRef,
			State:    string(ev.ToState),
		})
	}
	return events, nil
}
