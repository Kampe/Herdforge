// Package boardproj projects the durable Herdforge lifecycle onto a task
// board (Kaneo) so the board is a readable consequence of recorded truth
// rather than an independently-editable opinion.
//
// WHY (audit, 2026-08-02): the board showed zero cards In Review while five
// PRs and multiple reviewers existed; after recovery FAC-68 and FAC-84 stayed
// In Review after a REJECT while their workers were actively repairing; and
// FAC-119/FAC-121 sat in a generic In Progress while actually blocked on a
// dependency. Every one of those is the same bug: something other than the
// durable lifecycle log was allowed to decide what a card said, and nothing
// re-derived the card from the log afterwards.
//
// The rules this package enforces:
//   - Only a lifecycle state, plus the evidence that state's edge demands,
//     may move a card. Terminal titles, agent idle/done, PR existence and
//     green CI are not inputs here and never will be.
//   - Done is gated on a FAC-132 completion receipt AND a provider readback.
//     Absent a receipt the card holds at a safe non-Done column.
//   - A provider write that succeeds but reads back wrong is a hard
//     Recovering state, not a success.
package boardproj

import (
	"fmt"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// Managed labels. These are the ONLY labels this package attaches or
// detaches; anything else on a card belongs to a human or another system and
// is never touched.
const (
	LabelBlocked    = "herd:blocked"
	LabelRecovering = "herd:recovering"
)

// ManagedLabels is the closed set this package owns, in deterministic order.
func ManagedLabels() []string { return []string{LabelBlocked, LabelRecovering} }

func isManagedLabel(name string) bool {
	for _, l := range ManagedLabels() {
		if l == name {
			return true
		}
	}
	return false
}

// Projection is what one lifecycle state means on the board.
type Projection struct {
	// Status is the target board status, empty when CarryForward is set.
	Status string
	// CarryForward means the state does not itself name a column: Blocked
	// and Recovering keep whatever column the card already had (clamped away
	// from Done) and are represented by Label instead. Moving a blocked
	// reviewer-live card back to In Progress would be a second lie, not a
	// correction.
	CarryForward bool
	// Label is the managed label this state requires, or "" for none.
	Label string
	// DoneWithReceipt marks the post-integration states: Done is permitted
	// only with a valid FAC-132 completion receipt, and the card holds at
	// In Review until one exists.
	DoneWithReceipt bool
}

// Project maps a durable lifecycle state to its board projection. Unknown
// states fail closed: no status is guessed for a state this package has not
// been taught, because a guess is exactly how a card starts lying.
func Project(s lifecycle.State) (Projection, error) {
	switch s {
	case lifecycle.StateDraft, lifecycle.StateEligible:
		return Projection{Status: provider.StatusToDo}, nil
	case lifecycle.StateClaimed, lifecycle.StateDispatched,
		lifecycle.StateBuilding, lifecycle.StateVerifying:
		return Projection{Status: provider.StatusInProgress}, nil
	case lifecycle.StateReviewing, lifecycle.StateIntegrationQueued:
		return Projection{Status: provider.StatusInReview}, nil
	case lifecycle.StateIntegrated, lifecycle.StateReconciled, lifecycle.StateCleaned:
		return Projection{Status: provider.StatusInReview, DoneWithReceipt: true}, nil
	case lifecycle.StateBlocked:
		return Projection{CarryForward: true, Label: LabelBlocked}, nil
	case lifecycle.StateRecovering:
		return Projection{CarryForward: true, Label: LabelRecovering}, nil
	}
	return Projection{}, fmt.Errorf("%w: %q", ErrUnknownState, string(s))
}

// carryForward resolves a CarryForward projection against the previously
// applied status. Done is clamped to In Review: a card that has already been
// closed must not stay closed while the lifecycle says the work is blocked or
// recovering. With no prior projection, In Progress is the safe floor.
func carryForward(prior string) string {
	switch provider.NormalizeStatus(prior) {
	case provider.StatusToDo:
		return provider.StatusToDo
	case provider.StatusInReview, provider.StatusDone:
		return provider.StatusInReview
	case provider.StatusInProgress:
		return provider.StatusInProgress
	}
	return provider.StatusInProgress
}

// DeliveryRequired reports whether moving the card from prior to want is one
// of the two edges that a confirmed, exact-SHA delivery must authorize:
//
//   - entering In Review: the card may not claim a reviewer is looking at it
//     until the reviewer prompt for THAT candidate SHA was actually delivered
//     (the zero-In-Review audit finding is this edge never being taken; the
//     inverse — claiming review with no reviewer — is this edge being taken
//     on hope).
//   - leaving In Review for In Progress: on a FAIL the card may not move back
//     to the worker's column until the repair prompt was actually delivered.
//     FAC-68 and FAC-84 sat In Review through a REJECT because nothing tied
//     the column to the repair handoff.
//
// Both are computed from the previously APPLIED status rather than the event's
// from-state, so a card that reached In Review and then passed through
// Blocked or Recovering still needs delivery proof to leave it.
func DeliveryRequired(prior, want string) bool {
	prior = provider.NormalizeStatus(prior)
	want = provider.NormalizeStatus(want)
	switch {
	case want == provider.StatusInReview && prior != provider.StatusInReview:
		return true
	case want == provider.StatusInProgress && prior == provider.StatusInReview:
		return true
	}
	return false
}

// Delivery is proof that the prompt authorizing a column move actually
// reached its agent. It is derived from a pkg/textdelivery receipt plus the
// candidate SHA the prompt was built for; this package checks the binding, it
// does not perform the delivery.
type Delivery struct {
	// ReceiptKey and IntentSHA256 come from textdelivery.Receipt.
	ReceiptKey   string
	IntentSHA256 string
	// CandidateSHA is the exact SHA the delivered prompt named. A delivery
	// for a different SHA authorizes nothing: that is a stale handoff.
	CandidateSHA string
	// Generation is the lease generation the delivery was made under.
	Generation int64
}

// confirms reports whether d authorizes a column move for candidateSHA at
// generation. Every field is required — a partially-filled Delivery is an
// unproven one.
func (d *Delivery) confirms(candidateSHA string, generation int64) error {
	if d == nil {
		return fmt.Errorf("%w: no delivery supplied", ErrDeliveryUnconfirmed)
	}
	if d.ReceiptKey == "" || d.IntentSHA256 == "" {
		return fmt.Errorf("%w: delivery receipt key and intent digest are required", ErrDeliveryUnconfirmed)
	}
	if candidateSHA == "" || d.CandidateSHA != candidateSHA {
		return fmt.Errorf("%w: delivery names candidate %q, event names %q",
			ErrDeliveryUnconfirmed, d.CandidateSHA, candidateSHA)
	}
	if d.Generation != generation {
		return fmt.Errorf("%w: delivery is generation %d, event is generation %d",
			ErrDeliveryUnconfirmed, d.Generation, generation)
	}
	return nil
}

// Reason is the structured explanation a Blocked or Recovering card carries.
// A card that says "blocked" without saying by what, owned by whom, and what
// event would unblock it is the FAC-119/FAC-121 finding: visibly stuck, with
// nothing actionable on the card.
type Reason struct {
	Reason     string `json:"reason,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Dependency string `json:"dependency,omitempty"`
	NextEvent  string `json:"next_event,omitempty"`
}

func (r Reason) empty() bool {
	return r.Reason == "" && r.Owner == "" && r.Dependency == "" && r.NextEvent == ""
}
