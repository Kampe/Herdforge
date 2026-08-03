package control

import "context"

// RecipientLoop is the standing-lane receive boundary. The worker supplies an
// idempotency-aware processor; this package owns mailbox ordering and durable
// ack emission.
type RecipientLoop struct{ Consumer *Consumer }

func (l RecipientLoop) RunOnce(ctx context.Context) error {
	if l.Consumer == nil {
		return ErrProcessorUnavailable
	}
	return l.Consumer.Consume(ctx)
}

// CoordinatorLoop is the restart/pulse boundary. It reconciles only orders
// already present in the durable outbox and never creates phantom rows.
type CoordinatorLoop struct {
	Delivery *Delivery
	Orders   func(context.Context) ([]Order, error)
}

func (l CoordinatorLoop) RunOnce(ctx context.Context) error {
	if l.Delivery == nil || l.Orders == nil {
		return ErrMissingReceipt
	}
	orders, err := l.Orders(ctx)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if _, err := l.Delivery.Reconcile(ctx, order); err != nil {
			return err
		}
	}
	return nil
}
