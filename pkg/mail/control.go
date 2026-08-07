package mail

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// ControlSubjectPrefix marks a mailbox message whose body is a signed
// envelope.Envelope (JSON). Workers must route these through
// envelope.Session.ReceiveJSON — never treat the body as free-form prompt text.
//
// FAC-133: this is the production consumer path for trusted control
// provenance. Provider/card text uses ordinary subjects and stays untrusted.
const ControlSubjectPrefix = "herd.control/v1:"

// PostControl delivers a MAC-signed control envelope to a worker inbox.
// The mail-layer Envelope is transport only; authenticity lives in body.
// Fail-closed: nil control envelope, missing signature, or marshal error.
func (m *Mailbox) PostControl(sender, recipient string, ctrl *envelope.Envelope) (*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	if ctrl == nil {
		return nil, envelope.ErrNotControl
	}
	if err := ctrl.ValidateUnsigned(); err != nil {
		return nil, err
	}
	if ctrl.Signature == "" || !strings.HasPrefix(ctrl.Signature, "sha256=") {
		return nil, envelope.ErrInvalidSignature
	}
	body, err := json.Marshal(ctrl)
	if err != nil {
		return nil, fmt.Errorf("mail: marshal control envelope: %w", err)
	}
	subject := ControlSubjectPrefix + " " + string(ctrl.Kind) + " " + ctrl.TargetTask
	return m.SendMessage(sender, recipient, subject, string(body))
}

// IsControlSubject reports whether subject is a control-plane message.
func IsControlSubject(subject string) bool {
	return strings.HasPrefix(subject, ControlSubjectPrefix)
}

// AppliedControl is one control envelope drained and fed through Session.
type AppliedControl struct {
	MailID   string
	Decision *envelope.Decision
	Err      error
}

// DrainControl walks recipient's inbox, and for every control-subject message
// runs sess.ReceiveJSON. Non-control mail is skipped (shared inbox). The
// Session is the sole trust gate — this function never elevates free-form text.
//
// Fail-closed (FAC-133 admission): any rejected / replay / invalid control
// returns a non-nil error (callers exit nonzero). Applied slice still lists
// per-message decisions for diagnostics.
//
// Production consumer for pkg/envelope (FAC-133): coordinator Issue →
// PostControl → worker DrainControl(session).
func (m *Mailbox) DrainControl(recipient string, sess *envelope.Session) ([]AppliedControl, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	if sess == nil {
		return nil, envelope.ErrMissingBinding
	}
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return nil, err
	}
	var out []AppliedControl
	var firstFail error
	for _, e := range envs {
		if e == nil || !IsControlSubject(e.Subject) {
			continue
		}
		dec, rerr := sess.ReceiveJSON([]byte(e.Body))
		// StatusDuplicate of an already-applied envelope is success (at-least-once).
		// Rejected / blocked / replay / invalid MAC must fail closed.
		if rerr != nil {
			if firstFail == nil {
				firstFail = rerr
			}
		} else if dec != nil && dec.Status == envelope.StatusRejected {
			if firstFail == nil {
				firstFail = fmt.Errorf("mail: control rejected: %s", dec.Reason)
			}
		} else if dec != nil && dec.Status == envelope.StatusBlocked {
			if firstFail == nil {
				firstFail = fmt.Errorf("mail: session BLOCKED: %s", dec.Reason)
			}
		}
		out = append(out, AppliedControl{
			MailID:   e.ID,
			Decision: dec,
			Err:      rerr,
		})
	}
	return out, firstFail
}
