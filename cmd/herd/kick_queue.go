package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// surfaceQueuedAtKick is the FAC-773 idle-boundary consumer. Kick already
// re-engages idle/done lanes; this occupies that same turn with pending
// routine mail so a busy-queued message is surfaced without another
// operator send --drain.
func surfaceQueuedAtKick(name, workspace string) (bool, error) {
	path, err := controlMailPath("")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return surfaceQueuedAtKickMailbox(name, workspace, mail.NewMailbox(path))
}

func surfaceQueuedAtKickMailbox(name, workspace string, box *mail.Mailbox) (bool, error) {
	if box == nil {
		return false, errors.New("kick surface: mailbox is required")
	}
	results, err := herdr.DrainQueuedAtBoundary(name, workspace, box)
	occupied := false
	for _, result := range results {
		if result.Acknowledged {
			occupied = true
		}
	}
	if err != nil {
		if errors.Is(err, herdr.ErrNotIdleBoundary) {
			return occupied, nil
		}
		return occupied, err
	}
	return occupied, nil
}
