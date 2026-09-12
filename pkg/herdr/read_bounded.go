package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

// FAC-36: observation reads need bounds the mutating runners do not.
//
// runHerdrContextReal accumulates stdout and stderr into unbounded
// bytes.Buffers, and the non-context runHerdr passes context.Background(), so a
// read could hang forever and a runaway child could grow memory without limit.
// Neither is acceptable for a sweep that exists to observe a fleet cheaply.
//
// These helpers are deliberately scoped to READ paths. Mutating runners keep
// their existing behaviour: a bound that truncates a launch or a delivery is a
// different and more dangerous trade than one that truncates an observation.

const (
	// DefaultReadTransportLimit bounds one read's stdout at the process
	// boundary, before anything is buffered.
	DefaultReadTransportLimit = 1 << 20
	// readStderrLimit bounds diagnostic output. It is smaller because stderr
	// carries a message, not a payload.
	readStderrLimit = 64 << 10
)

var (
	// ErrReadTransportOversized means the child produced more than the read
	// bound allows. The child is stopped; the output is refused rather than
	// silently truncated into something that looks like a complete answer.
	ErrReadTransportOversized = errors.New("herdr read: transport output exceeded its bound")
	// ErrReadTransportEnvelope means herdr itself reported a failure, as
	// distinct from an agent whose own output happens to look like an error.
	ErrReadTransportEnvelope = errors.New("herdr read: transport reported an error")
	// ErrReadTransportContradiction means one response claimed both an error
	// and a result. Guessing which half to believe is how an error envelope
	// carrying a result array gets consumed as data.
	ErrReadTransportContradiction = errors.New("herdr read: response carried both an error and a result")
	// ErrReadTransportMalformed means the reply announced itself as a
	// structured response and then could not be decoded. Falling back to
	// "probably raw text" here is what let a truncated reply be classified as
	// a healthy pane tail.
	ErrReadTransportMalformed = errors.New("herdr read: structured response could not be decoded")
	// ErrReadTransportUnsupportedResult means the envelope decoded but its
	// result body is not a shape this client knows how to read. An unknown
	// body is an unread pane, not an empty one.
	ErrReadTransportUnsupportedResult = errors.New("herdr read: result body shape is not supported")
)

// cappedWriter accepts at most limit bytes and then calls onOver exactly once.
// Write never returns an error: erroring here races the copy goroutine against
// Wait, and cancelling the child is both the stronger and the simpler stop.
type cappedWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	limit  int
	over   bool
	onOver func()
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.over {
		w.mu.Unlock()
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) <= remaining {
		w.buf.Write(p)
		w.mu.Unlock()
		return len(p), nil
	}
	w.buf.Write(p[:remaining])
	w.over = true
	stop := w.onOver
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
	return len(p), nil
}

func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *cappedWriter) overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.over
}

// runHerdrReadContextReal runs one observation child under ctx with both
// streams bounded at the process boundary.
//
// Cancellation stops only this child: procsignal.CommandContext puts it in its
// own process group and its Cancel reaps that group, with a WaitDelay so a
// process ignoring the signal cannot hold the caller. No goroutine is detached
// to work around a blocking read.
func runHerdrReadContextReal(ctx context.Context, limit int, args ...string) (string, error) {
	bin, binErr := binaryPath()
	if binErr != nil {
		return "", binErr
	}
	if limit <= 0 {
		limit = DefaultReadTransportLimit
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := procsignal.CommandContext(runCtx, bin, args...)
	stdout := &cappedWriter{limit: limit, onOver: cancel}
	stderr := &cappedWriter{limit: readStderrLimit, onOver: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	label := "herdr"
	if len(args) > 0 {
		label = args[0]
	}
	// Oversize is reported before the exit status: a child killed BECAUSE it
	// overran must not be reported as an ordinary non-zero exit.
	if stdout.overflowed() || stderr.overflowed() {
		return "", fmt.Errorf("%w: %s exceeded %d bytes and was stopped", ErrReadTransportOversized, label, limit)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("herdr read %s: %w", label, ctxErr)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return "", runErr
		}
		return "", fmt.Errorf("%w: %s", runErr, msg)
	}
	return stdout.String(), nil
}

// runHerdrReadContext dispatches exactly as runHerdrContext does, so every
// existing test override keeps working; only the real path gains the bound.
func runHerdrReadContext(ctx context.Context, limit int, args ...string) (string, error) {
	if runHerdrContextOverride != nil {
		return runHerdrContextOverride(ctx, args...)
	}
	if reflect.ValueOf(runHerdr).Pointer() != reflect.ValueOf(runHerdrReal).Pointer() {
		return runHerdr(args...)
	}
	return runHerdrReadContextReal(ctx, limit, args...)
}

// herdrEnvelope is the transport shape. Result and Error are RawMessage so
// "absent" and "present but null" stay distinguishable.
//
// Verified against the installed herdr (0.9.0, `herdr api schema --json`,
// schema_version 1): success_response REQUIRES {id, result} and error_response
// REQUIRES {id, error:{code,message}}. There is no "ok" key in either; it is
// accepted here only because this repository's own fixtures emit it, and never
// as the sole marker of success.
type herdrEnvelope struct {
	ID     json.RawMessage `json:"id"`
	OK     *bool           `json:"ok"`
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
}

func presentAndNotNull(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// decodeHerdrTransport separates a TRANSPORT reply from raw pane text, and
// fails closed on everything in between.
//
// The line is drawn by POSITION, not by key names. A reply that begins with
// '{' is herdr speaking, and is judged as transport: if it does not decode, or
// decodes without a usable result, the read FAILED. An "error" key at the top
// level is herdr's error envelope. The same object nested inside result.text
// is the agent's own output and is preserved verbatim — that is the direction
// that must not regress, and it is handled by never inspecting decoded content.
//
// Two earlier versions of this were wrong in opposite directions. Sniffing the
// decoded agent text for an "error" key mislabelled a healthy pane whose
// literal output is {"error":"example"}. Then requiring a "result" or "ok"
// marker let a bare {"error":"failure"} and a truncated structured reply fall
// through to the raw-text path and be classified as a healthy pane tail. Only
// output that never claimed to be structured reaches that path now, and the
// caller is told it is unverified.
func decodeHerdrTransport(raw string) (result json.RawMessage, isEnvelope bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed[0] != '{' {
		// Never claimed to be a structured reply. Legacy raw text: usable, but
		// nothing in it proves the read succeeded.
		return nil, false, nil
	}
	var envelope herdrEnvelope
	if decErr := json.Unmarshal([]byte(trimmed), &envelope); decErr != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrReadTransportMalformed, decErr)
	}
	hasResult := presentAndNotNull(envelope.Result)
	hasError := presentAndNotNull(envelope.Error)
	switch {
	case hasError && hasResult:
		return nil, true, fmt.Errorf("%w: %s", ErrReadTransportContradiction, strings.TrimSpace(string(envelope.Error)))
	case hasError:
		return nil, true, fmt.Errorf("%w: %s", ErrReadTransportEnvelope, strings.TrimSpace(string(envelope.Error)))
	case envelope.OK != nil && !*envelope.OK:
		return nil, true, fmt.Errorf("%w: response reported ok=false", ErrReadTransportEnvelope)
	case hasResult:
		return envelope.Result, true, nil
	}
	// A structured reply carrying neither. Against the 0.9.0 contract it is
	// neither a success nor an error, so the pane was not read — including the
	// case of a top-level object that is really agent output, which cannot be
	// distinguished from a malformed envelope and must not be given the
	// benefit of the doubt.
	if presentAndNotNull(envelope.ID) {
		return nil, true, fmt.Errorf("%w: response carried an id but neither a result nor an error", ErrReadTransportEnvelope)
	}
	return nil, true, fmt.Errorf("%w: top-level object is neither a result nor an error envelope", ErrReadTransportEnvelope)
}

// PaneObservation is one bounded pane read. It exists so the caller is not
// handed a bare string that cannot say how much to trust it.
type PaneObservation struct {
	// Text is the pane tail.
	Text string
	// Truncated is herdr's OWN statement that it cut the tail. PaneReadResult
	// carries it as a required field, and a truncated tail is different
	// evidence from a complete one, so it is preserved rather than dropped.
	Truncated bool
	// Unverified is true when the reply never claimed to be a structured herdr
	// response — legacy raw text. The text is returned because it is usable,
	// but nothing in it proved the read succeeded, so a caller must not
	// present it as a healthy observation without saying so.
	Unverified bool
}

// PaneReadContext is PaneRead bounded by ctx and by a transport byte limit,
// with the transport reply validated BEFORE any pane text is extracted.
//
// Result bodies are accepted in exactly the two shapes this repository knows:
// herdr 0.9.0's PaneReadResult, whose "text" is required, and the older
// "lines" array pane.go already accommodates. Anything else fails closed —
// an unrecognised body is an unread pane, not an empty one.
func PaneReadContext(ctx context.Context, paneID string, lines int) (PaneObservation, error) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return PaneObservation{}, fmt.Errorf("herdr pane read: pane id is required")
	}
	if lines <= 0 {
		lines = 80
	}
	out, err := runHerdrReadContext(ctx, DefaultReadTransportLimit,
		"pane", "read", paneID, "--source", "recent-unwrapped", "--lines", fmt.Sprint(lines))
	if err != nil {
		return PaneObservation{}, fmt.Errorf("herdr pane read %s: %w", paneID, err)
	}
	result, isEnvelope, decodeErr := decodeHerdrTransport(out)
	if decodeErr != nil {
		return PaneObservation{}, fmt.Errorf("herdr pane read %s: %w", paneID, decodeErr)
	}
	if !isEnvelope {
		return PaneObservation{Text: out, Unverified: true}, nil
	}
	// Pointer and nil-slice presence, not emptiness: an envelope that reports
	// an EMPTY pane and one whose body this code does not recognise are
	// different facts, and collapsing both to "" discards the second.
	var body struct {
		Text      *string  `json:"text"`
		Lines     []string `json:"lines"`
		Truncated bool     `json:"truncated"`
	}
	if unmarshalErr := json.Unmarshal(result, &body); unmarshalErr != nil {
		return PaneObservation{}, fmt.Errorf("herdr pane read %s: %w: %v",
			paneID, ErrReadTransportUnsupportedResult, unmarshalErr)
	}
	switch {
	case body.Text != nil:
		return PaneObservation{Text: *body.Text, Truncated: body.Truncated}, nil
	case body.Lines != nil:
		return PaneObservation{Text: strings.Join(body.Lines, "\n"), Truncated: body.Truncated}, nil
	}
	return PaneObservation{}, fmt.Errorf("herdr pane read %s: %w: result has neither text nor lines",
		paneID, ErrReadTransportUnsupportedResult)
}
