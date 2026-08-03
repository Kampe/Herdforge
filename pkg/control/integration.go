package control

import (
	"context"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// HerdrWaker is the only Herdr integration used by the durable adapter. Its
// receipt proves prompt consumption only; it never acknowledges or finalizes
// the durable order.
type HerdrWaker struct {
	Target  string
	Timeout time.Duration
}

func (w HerdrWaker) Wake(_ context.Context, req WakeRequest) (WakeReceipt, error) {
	if w.Target == "" {
		return WakeReceipt{}, fmt.Errorf("control: Herdr target is required")
	}
	receipt, err := herdr.DeliverAndProve(w.Target, fmt.Sprintf("consume durable control envelope %s seq %d", req.MessageID, req.Sequence), w.Timeout)
	if err != nil {
		return WakeReceipt{}, err
	}
	if receipt == nil || !receipt.Consumed {
		return WakeReceipt{}, ErrMissingReceipt
	}
	return WakeReceipt{MessageID: req.MessageID, Consumed: true}, nil
}

// CoordinatorOrders is the production-facing order port used by dispatch,
// review, and review-supervisor flows. It carries the authoritative identity
// context once, so callers cannot manufacture a wake-only prompt without a
// durable order.
type CoordinatorOrders struct {
	Delivery *Delivery
	Identity LaneIdentity
}

func (c *CoordinatorOrders) send(ctx context.Context, kind Kind, body string) (Evidence, error) {
	if c == nil || c.Delivery == nil {
		return Evidence{}, ErrMissingReceipt
	}
	o := Order{LaneIdentity: c.Identity, Kind: kind, Body: body}
	return c.Delivery.Deliver(ctx, o)
}

func (c *CoordinatorOrders) Rebase(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindRebase, body)
}
func (c *CoordinatorOrders) Repair(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindRepair, body)
}
func (c *CoordinatorOrders) ReviewCorrection(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindReviewCorrection, body)
}
func (c *CoordinatorOrders) Callback(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindCallback, body)
}
