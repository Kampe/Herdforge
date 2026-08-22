package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FAC-569: Envelope.Read exists but nothing ever set it, so a "pending" filter
// on !env.Read matched everything forever. A queue whose acknowledgement is a
// field nobody writes is not a queue -- it is a log that looks like one.
//
// The mailbox is append-only, so handled state lives beside it in a small
// side-file, the same shape the callback consumer already uses for its own
// progress. One definition, so "handled" cannot come to mean two things.

// ackPath is the handled-state file for a mailbox.
func ackPath(mailFile string) string {
	return mailFile + ".handled.json"
}

type ackState struct {
	// Handled maps recipient -> envelope ids that reached a disposition.
	Handled map[string][]string `json:"handled"`
}

func loadAck(mailFile string) (*ackState, error) {
	data, err := os.ReadFile(ackPath(mailFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &ackState{Handled: map[string][]string{}}, nil
		}
		return nil, err
	}
	var st ackState
	if err := json.Unmarshal(data, &st); err != nil {
		// Fail closed: a corrupt ack file must not read as "nothing handled"
		// and re-deliver settled work, nor as "all handled" and drop live work.
		return nil, fmt.Errorf("parse handled state: %w", err)
	}
	if st.Handled == nil {
		st.Handled = map[string][]string{}
	}
	return &st, nil
}

// MarkHandled records that one envelope reached a disposition.
//
// This is deliberately NOT "mark read". Reading a handoff is not finishing it,
// and conflating the two is how a queue silently drains itself: an entry must
// stay pending until its work has an outcome.
func (m *Mailbox) MarkHandled(recipient, id string) error {
	recipient, id = strings.TrimSpace(recipient), strings.TrimSpace(id)
	if recipient == "" || id == "" {
		return fmt.Errorf("mail: recipient and envelope id are required")
	}
	st, err := loadAck(m.MailFile)
	if err != nil {
		return err
	}
	for _, known := range st.Handled[recipient] {
		if known == id {
			return nil // idempotent
		}
	}
	st.Handled[recipient] = append(st.Handled[recipient], id)
	sort.Strings(st.Handled[recipient])

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := ackPath(m.MailFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".handled-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// Handled reports whether an envelope already reached a disposition.
func (m *Mailbox) Handled(recipient, id string) (bool, error) {
	st, err := loadAck(m.MailFile)
	if err != nil {
		return false, err
	}
	for _, known := range st.Handled[strings.TrimSpace(recipient)] {
		if known == strings.TrimSpace(id) {
			return true, nil
		}
	}
	return false, nil
}
