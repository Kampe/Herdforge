package herdr

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// sendRecorder captures the herdr invocations a send makes.
type sendRecorder struct {
	mu     sync.Mutex
	calls  []string
	status string
	// pane is returned once the send has happened; baselinePane is returned for
	// the pre-send read. Consumption is measured as an INCREASE in occurrences,
	// so a fixture that shows the text before the send proves nothing.
	pane         string
	baselinePane string
	sent         bool
}

func (r *sendRecorder) run(args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(args, " "))
	r.mu.Unlock()
	switch {
	case len(args) > 1 && args[0] == "agent" && args[1] == "list":
		return `{"result":{"type":"agents","agents":[{"name":"lane","agent_status":"` +
			r.status + `","tab_id":"wK:t1","pane_id":"wK:p1","workspace_id":"wK","kind":"opencode"}]}}`, nil
	case len(args) > 1 && args[0] == "pane" && args[1] == "read":
		body := r.baselinePane
		r.mu.Lock()
		sent := r.sent
		r.mu.Unlock()
		if sent {
			body = r.pane
		}
		return `{"result":{"type":"pane","content":` + jsonQuote(body) + `}}`, nil
	case len(args) > 1 && args[0] == "agent" && (args[1] == "prompt" || args[1] == "send"):
		r.mu.Lock()
		r.sent = true
		r.mu.Unlock()
	}
	return `{"result":{"type":"ok"}}`, nil
}

func (r *sendRecorder) enterCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, "send-keys") && strings.Contains(c, "Enter") {
			n++
		}
	}
	return n
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestConfirmedSendSubmitsExactlyOnce is FAC-419's explicit verification.
//
// FAC-388 added an immediate Enter right after the prompt so a following status
// poll does not observe text stranded in the composer. The card asks whether
// that can DOUBLE-submit once the polling nudge also fires.
//
// It cannot, and the mechanism is worth stating plainly: the immediate Enter
// marks the send as already nudged, which means the later halfway nudge never
// fires AT ALL. One Enter, then fail closed. That is the correct trade for an
// instruction — at-most-once beats retry-until-consumed, because a double
// submit delivers the same instruction twice and cannot be undone.
func TestConfirmedSendSubmitsExactlyOnce(t *testing.T) {
	text := "do the thing"
	rec := &sendRecorder{
		status:       "working",
		baselinePane: "prompt> ",
		pane:         "prompt> " + text + "\nThinking...",
	}
	restore := SetRunHerdrForTest(rec.run)
	defer restore()

	status, err := SendInWorkspace("lane", text, true, 10*time.Second, "wK")
	if err != nil {
		t.Fatalf("a consumed send must succeed: %v", err)
	}
	if status != "working" && status != "done" {
		t.Fatalf("status = %q, want a consumed status", status)
	}
	if got := rec.enterCount(); got != 1 {
		t.Errorf("exactly one Enter must be sent on the happy path, got %d: %v", got, rec.calls)
	}
}

// TestUnconsumedSendFailsClosed is FAC-451's requirement.
//
// The reported failure is a send that lands in the composer, never submits, and
// reports success anyway — so the caller moves on and the instruction is lost.
// An unconfirmed delivery must be an ERROR, because `herd send ... && next-step`
// is exactly how a silent non-delivery becomes a skipped step.
func TestUnconsumedSendFailsClosed(t *testing.T) {
	// Pane never shows the text: consumption is never observed.
	rec := &sendRecorder{
		status:       "idle",
		baselinePane: "prompt> ",
		pane:         "prompt> [Pasted Content 412 chars]",
	}
	restore := SetRunHerdrForTest(rec.run)
	defer restore()

	status, err := SendInWorkspace("lane", "a long multi-line instruction", true, 3*time.Second, "wK")
	if err == nil {
		t.Fatal("a send whose consumption was never observed must NOT report success")
	}
	// The guard matters HERE, not on the happy path: this is the run where the
	// halfway nudge would fire if the immediate Enter had not already claimed
	// it. At-most-once must hold even when delivery fails.
	if got := rec.enterCount(); got != 1 {
		t.Errorf("at most one Enter may ever be sent, got %d: %v", got, rec.calls)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued so a caller can tell staged from delivered", status)
	}
	for _, want := range []string{"queued-but-not-consumed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name the condition (%q), got: %v", want, err)
		}
	}
}

// The opt-out must be visibly unverified rather than quietly indistinguishable
// from a confirmed delivery.
func TestNoVerifyIsLabelledUnverified(t *testing.T) {
	rec := &sendRecorder{status: "idle"}
	restore := SetRunHerdrForTest(rec.run)
	defer restore()

	status, err := SendInWorkspace("lane", "text", false, time.Second, "wK")
	if err != nil {
		t.Fatalf("--no-verify is an explicit opt-out, not a failure: %v", err)
	}
	if status != "submitted" {
		t.Fatalf("status = %q, want submitted", status)
	}
	if !strings.Contains(FormatSendResultInWorkspace("lane", "wK", status), "UNVERIFIED") {
		t.Error("an unverified send must say so in its human line")
	}
}
