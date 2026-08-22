package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// FAC-567: sequential delivery to the single review supervisor let a later
// handoff interrupt and SUPERSEDE an earlier one. Two real queues -- 29 API
// commits and 2 DeFi commits -- could not both be delivered safely, and a
// repeated pulse --act reproduced the same starvation loop.
//
// This is the delivery-side twin of FAC-533: consumption is not retention. Text
// that reached a pane can still be displaced by the next thing written there.
//
// The fix is durability, not refusal. Both handoffs are real work, so rejecting
// the second would trade lost work for undelivered work. Every handoff is
// appended to the durable mailbox FIRST; the pane notification is a nudge on top
// of a record that already exists. The supervisor drains its inbox at its next
// idle point and neither queue is lost.

// handoffEnvelopeID derives a stable id from the lane and its exact candidate
// set, so the append is idempotent.
//
// Repeated pulse --act with an unchanged candidate set must not enqueue a
// duplicate, and a CHANGED set must enqueue a new entry -- new work is not the
// same handoff. Keying on the lane alone would suppress genuinely new
// candidates; keying on a timestamp would duplicate on every beat.
func handoffEnvelopeID(lane string, commits []harvest.UnlandedCommit) string {
	shas := make([]string, 0, len(commits))
	for _, c := range commits {
		shas = append(shas, c.SHA)
	}
	sort.Strings(shas)
	sum := sha256.Sum256([]byte(strings.TrimSpace(lane) + "\n" + strings.Join(shas, "\n")))
	return "pulse-review-" + hex.EncodeToString(sum[:])[:16]
}

// enqueueReviewHandoff durably records a handoff for recipient.
//
// It returns whether the entry was newly appended. An already-present id is not
// an error: it means this exact handoff is still pending, which is precisely the
// state a repeated beat should leave alone rather than duplicate.
func enqueueReviewHandoff(ctx context.Context, mailPath, sender, recipient, subject, body, id string) (bool, error) {
	box := mail.NewMailbox(mailPath)

	// Ask whether this exact handoff is already queued, rather than inferring it
	// from an error message. The first version string-matched "duplicate" or
	// "already" in the append error -- a guess about wording that was simply
	// wrong: the append does not error on a repeated id at all, so every beat
	// reported new work. Matching on prose is not a contract.
	existing, err := box.ReadInbox(recipient)
	if err != nil {
		return false, fmt.Errorf("read pending handoffs for %s: %w", recipient, err)
	}
	for _, env := range existing {
		if env.ID == id && !env.Read {
			return false, nil
		}
	}

	env := &mail.Envelope{
		ID:        id,
		Sender:    sender,
		Recipient: recipient,
		Subject:   subject,
		Body:      body,
	}
	if err := box.AppendEnvelopeContext(ctx, env); err != nil {
		return false, fmt.Errorf("durably queue review handoff for %s: %w", recipient, err)
	}
	return true, nil
}

// PendingReviewHandoffs lists a recipient's undelivered handoffs, so the pending
// set is inspectable rather than inferred from pane contents.
func PendingReviewHandoffs(mailPath, recipient string) ([]*mail.Envelope, error) {
	box := mail.NewMailbox(mailPath)
	all, err := box.ReadInbox(recipient)
	if err != nil {
		return nil, err
	}
	var pending []*mail.Envelope
	for _, env := range all {
		if env.Read || !strings.HasPrefix(env.ID, "pulse-review-") {
			continue
		}
		pending = append(pending, env)
	}
	return pending, nil
}
