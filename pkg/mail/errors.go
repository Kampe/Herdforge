package mail

import (
	"errors"
	"fmt"
)

// ErrRecipientAndEnvelopeIDRequired is the shared identity validation used by
// append/ack/status callers. Keeping this wording in one package definition
// prevents the mailbox contract from drifting between consumers.
var ErrRecipientAndEnvelopeIDRequired = errors.New("mail: recipient and envelope id are required")

func ordinaryEnvelopeClassError(id string) error {
	return fmt.Errorf("mail: envelope %q is not an ordinary report", id)
}

func ordinaryEnvelopeNotFound(id, recipient string) error {
	return fmt.Errorf("mail: ordinary envelope %q for recipient %q was not found", id, recipient)
}
