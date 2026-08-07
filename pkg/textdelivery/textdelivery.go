// Package textdelivery delivers arbitrary text without shell interpretation.
package textdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrEmptyExecutable  = errors.New("textdelivery: executable is empty")
	ErrInvalidPayload   = errors.New("textdelivery: payload must specify exactly one source")
	ErrReadbackMismatch = errors.New("textdelivery: readback does not match payload")
	ErrReplayMismatch   = errors.New("textdelivery: replay payload digest differs")
	ErrEmptyKey         = errors.New("textdelivery: delivery key is empty")
	ErrDurableAmbiguous = errors.New("textdelivery: accepted delivery has no completion proof")
	ErrDurableCorrupt   = errors.New("textdelivery: durable receipt is corrupt or missing")
	ErrNULPayload       = errors.New("textdelivery: payload contains NUL and cannot be an argv element")
)

// Payload is either an in-memory byte sequence or a file-backed byte sequence.
// The two sources are mutually exclusive, and bytes are never interpreted as text.
type Payload struct {
	Bytes []byte
	File  string
}

func (p Payload) read() ([]byte, error) {
	if (p.Bytes == nil) == (p.File == "") {
		return nil, ErrInvalidPayload
	}
	if p.File != "" {
		return os.ReadFile(p.File)
	}
	return append([]byte(nil), p.Bytes...), nil
}

// Read returns an immutable copy of the selected payload source.
func (p Payload) Read() ([]byte, error) { return p.read() }

// Command is the exact direct-argv invocation supplied to an Executor.
type Command struct {
	Executable string
	Args       []string
	Payload    []byte
}

// Executor is the transport seam. Implementations must deliver Command.Payload
// as stdin to Command.Executable with Command.Args, without a shell.
type Executor interface {
	Execute(context.Context, Command) ([]byte, error)
}

// ExecutorFunc adapts a compiled transport function to Executor. The command
// is still supplied as structured executable/argv/payload data; callers never
// need to construct a shell command string.
type ExecutorFunc func(context.Context, Command) ([]byte, error)

func (f ExecutorFunc) Execute(ctx context.Context, command Command) ([]byte, error) {
	if f == nil {
		return nil, errors.New("textdelivery: nil executor function")
	}
	return f(ctx, command)
}

// ReadbackPolicy validates a successful transport's returned bytes. The
// policy is sealed into a Ledger at construction and cannot drift while it is
// serving concurrent deliveries.
type ReadbackPolicy func(payload, readback []byte) bool

// Receipt is created only after successful, byte-identical readback.
type Receipt struct {
	Key          string
	SHA256       string
	IntentSHA256 string
	Readback     []byte
	Generation   int64
}

// DurableReceiptStore is the cross-process authority. Reserve must persist
// acceptance before Execute; Complete must use a durable CAS from accepted
// to completed. Implementations must reject missing, corrupt, or conflicting
// state rather than treating it as a new delivery.
type DurableReceiptStore interface {
	ReserveDelivery(DeliveryIntent) (DurableReceipt, error)
	CompleteDelivery(DeliveryIntent, []byte) (DurableReceipt, error)
}

type DeliveryIntent struct {
	Key, Executable             string
	Args                        []string
	PayloadSHA256, IntentSHA256 string
	Generation                  int64
}

type DurableReceipt struct {
	Key, Executable             string
	Args                        []string
	PayloadSHA256, IntentSHA256 string
	Generation                  int64
	Readback                    []byte
	ReadbackSHA256              string
	Completed                   bool
}

type deliveryFlight struct {
	intent  string
	done    chan struct{}
	receipt Receipt
	err     error
}

// Ledger makes successful deliveries idempotent and rejects mismatched replays.
type Ledger struct {
	mu             sync.Mutex
	receipts       map[string]Receipt
	inFlight       map[string]*deliveryFlight
	onWaiterAdmit  func()
	readbackPolicy ReadbackPolicy
	durable        DurableReceiptStore
	durableMode    bool
	generation     int64
}

func NewLedger() *Ledger {
	return &Ledger{receipts: make(map[string]Receipt), inFlight: make(map[string]*deliveryFlight)}
}

// NewLedgerWithReadbackPolicy constructs a ledger with an immutable transport
// readback policy. A nil policy retains the foundation's exact-byte rule.
func NewLedgerWithReadbackPolicy(policy ReadbackPolicy) *Ledger {
	l := NewLedger()
	l.readbackPolicy = policy
	return l
}

// NewDurableLedger binds a Ledger to a shared durable receipt authority and a
// positive session generation. It is safe to construct one instance per
// process; the authority, not this process, owns replay and CAS decisions.
func NewDurableLedger(store DurableReceiptStore, generation int64) *Ledger {
	return NewDurableLedgerWithReadbackPolicy(store, generation, nil)
}

// NewDurableLedgerWithReadbackPolicy binds a Ledger to a shared durable
// receipt authority and validates transport-specific completion readbacks.
// The policy is immutable for the lifetime of the ledger.
func NewDurableLedgerWithReadbackPolicy(store DurableReceiptStore, generation int64, policy ReadbackPolicy) *Ledger {
	l := NewLedger()
	l.durable = store
	l.durableMode = true
	l.generation = generation
	l.readbackPolicy = policy
	return l
}

// Deliver reads the payload, executes the direct-argv command, verifies exact
// readback, and records a receipt. Failed executions never create a receipt.
func (l *Ledger) Deliver(ctx context.Context, key, executable string, args []string, payload Payload, executor Executor) (Receipt, error) {
	return l.deliver(ctx, key, executable, args, payload, nil, executor)
}

// DeliverBound is Deliver with an additional opaque caller identity binding.
// The binding authenticates the durable intent but is never passed to the
// executor, argv, or stdin. A nil binding preserves the legacy intent hash.
func (l *Ledger) DeliverBound(ctx context.Context, key, executable string, args []string, payload Payload, binding []byte, executor Executor) (Receipt, error) {
	return l.deliver(ctx, key, executable, args, payload, append([]byte(nil), binding...), executor)
}

func (l *Ledger) deliver(ctx context.Context, key, executable string, args []string, payload Payload, binding []byte, executor Executor) (Receipt, error) {
	if l == nil {
		return Receipt{}, errors.New("textdelivery: nil ledger")
	}
	if executor == nil {
		return Receipt{}, errors.New("textdelivery: nil executor")
	}
	if key == "" {
		return Receipt{}, ErrEmptyKey
	}
	if executable == "" {
		return Receipt{}, ErrEmptyExecutable
	}
	body, err := payload.read()
	if err != nil {
		return Receipt{}, err
	}
	digest := Digest(body)
	intent := intentDigest(key, executable, args, digest)
	if binding != nil {
		intent = boundIntentDigest(key, executable, args, digest, binding)
	}
	if l.durableMode {
		return l.deliverDurable(ctx, key, executable, args, body, digest, intent, executor)
	}

	l.mu.Lock()
	if l.receipts == nil {
		l.receipts = make(map[string]Receipt)
	}
	if l.inFlight == nil {
		l.inFlight = make(map[string]*deliveryFlight)
	}
	if prior, ok := l.receipts[key]; ok {
		if prior.IntentSHA256 != intent {
			l.mu.Unlock()
			return Receipt{}, fmt.Errorf("%w for %q", ErrReplayMismatch, key)
		}
		l.mu.Unlock()
		return cloneReceipt(prior), nil
	}
	if flight, ok := l.inFlight[key]; ok {
		if l.onWaiterAdmit != nil {
			l.onWaiterAdmit()
		}
		if flight.intent != intent {
			l.mu.Unlock()
			return Receipt{}, fmt.Errorf("%w for %q", ErrReplayMismatch, key)
		}
		l.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return Receipt{}, err
		}
		select {
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return Receipt{}, err
			}
			return cloneReceipt(flight.receipt), flight.err
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		}
	}
	flight := &deliveryFlight{intent: intent, done: make(chan struct{})}
	l.inFlight[key] = flight
	l.mu.Unlock()

	readback, err := executor.Execute(ctx, Command{Executable: executable, Args: append([]string(nil), args...), Payload: body})
	validReadback := bytes.Equal(body, readback)
	if l.readbackPolicy != nil {
		validReadback = l.readbackPolicy(body, readback)
	}
	if err == nil && !validReadback {
		err = ErrReadbackMismatch
	}
	receipt := Receipt{Key: key, SHA256: digest, IntentSHA256: intent, Readback: append([]byte(nil), readback...)}

	l.mu.Lock()
	if err == nil {
		l.receipts[key] = cloneReceipt(receipt)
	}
	if err == nil {
		flight.receipt = cloneReceipt(receipt)
	} else {
		flight.receipt = Receipt{}
	}
	flight.err = err
	delete(l.inFlight, key)
	close(flight.done)
	l.mu.Unlock()
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (l *Ledger) deliverDurable(ctx context.Context, key, executable string, args []string, body []byte, digest, intent string, executor Executor) (Receipt, error) {
	if l.durable == nil || l.generation <= 0 {
		return Receipt{}, ErrDurableCorrupt
	}
	durableIntent := DeliveryIntent{Key: key, Executable: executable, Args: append([]string(nil), args...), PayloadSHA256: digest, IntentSHA256: intent, Generation: l.generation}
	prior, err := l.durable.ReserveDelivery(durableIntent)
	if err == nil && prior.Completed {
		validReadback := bytes.Equal(body, prior.Readback)
		if l.readbackPolicy != nil {
			validReadback = l.readbackPolicy(body, prior.Readback)
		}
		if !validReadback {
			return Receipt{}, ErrReadbackMismatch
		}
		return Receipt{Key: key, SHA256: prior.PayloadSHA256, IntentSHA256: prior.IntentSHA256, Readback: append([]byte(nil), prior.Readback...), Generation: prior.Generation}, nil
	}
	if err != nil {
		return Receipt{}, err
	}
	readback, execErr := executor.Execute(ctx, Command{Executable: executable, Args: append([]string(nil), args...), Payload: body})
	valid := bytes.Equal(body, readback)
	if l.readbackPolicy != nil {
		valid = l.readbackPolicy(body, readback)
	}
	if execErr != nil {
		return Receipt{}, execErr
	}
	if !valid {
		return Receipt{}, ErrReadbackMismatch
	}
	completed, err := l.durable.CompleteDelivery(durableIntent, readback)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Key: key, SHA256: completed.PayloadSHA256, IntentSHA256: completed.IntentSHA256, Readback: append([]byte(nil), completed.Readback...), Generation: completed.Generation}, nil
}

func cloneReceipt(r Receipt) Receipt {
	r.Readback = append([]byte(nil), r.Readback...)
	return r
}

// Digest returns the lowercase hexadecimal SHA-256 of payload.
func Digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func intentDigest(key, executable string, args []string, payloadDigest string) string {
	h := sha256.New()
	writePart := func(value string) {
		io.WriteString(h, strconv.Itoa(len(value)))
		io.WriteString(h, ":")
		io.WriteString(h, value)
	}
	writePart(key)
	writePart(executable)
	io.WriteString(h, strconv.Itoa(len(args)))
	io.WriteString(h, ":")
	for _, arg := range args {
		writePart(arg)
	}
	writePart(payloadDigest)
	return hex.EncodeToString(h.Sum(nil))
}

func boundIntentDigest(key, executable string, args []string, payloadDigest string, binding []byte) string {
	h := sha256.New()
	writePart := func(value string) {
		io.WriteString(h, strconv.Itoa(len(value)))
		io.WriteString(h, ":")
		io.WriteString(h, value)
	}
	writePart(key)
	writePart(executable)
	io.WriteString(h, strconv.Itoa(len(args)))
	io.WriteString(h, ":")
	for _, arg := range args {
		writePart(arg)
	}
	writePart(payloadDigest)
	io.WriteString(h, "binding:")
	io.WriteString(h, strconv.Itoa(len(binding)))
	io.WriteString(h, ":")
	_, _ = h.Write(binding)
	return hex.EncodeToString(h.Sum(nil))
}

// IntentDigest returns the authenticated digest for a delivery operation.
func IntentDigest(key, executable string, args []string, payloadDigest string) string {
	return intentDigest(key, executable, args, payloadDigest)
}

// BoundIntentDigest returns the authenticated intent digest including opaque
// caller identity bytes while leaving executable argv unchanged.
func BoundIntentDigest(key, executable string, args []string, payloadDigest string, binding []byte) string {
	return boundIntentDigest(key, executable, args, payloadDigest, binding)
}

// Process is the minimal process seam used by DirectExecutor.
type Process interface {
	SetStdin(io.Reader)
	Output() ([]byte, error)
}

// CommandStarter constructs a process for a direct executable and argv.
type CommandStarter func(context.Context, string, ...string) Process

// DirectExecutor uses an injectable process starter and never invokes a shell.
type DirectExecutor struct{ Start CommandStarter }

func NewDirectExecutor(start CommandStarter) DirectExecutor {
	if start == nil {
		start = func(ctx context.Context, executable string, args ...string) Process {
			return &osProcess{cmd: exec.CommandContext(ctx, executable, args...)}
		}
	}
	return DirectExecutor{Start: start}
}

// ErrShellExecutable is returned when the transport would invoke a shell.
var ErrShellExecutable = errors.New("textdelivery: shell executables are forbidden for free-form payloads")

// IsShellExecutable reports whether name is a known shell that must never
// receive free-form coordinator text as a command string (FAC-151 / FAC-183).
func IsShellExecutable(name string) bool {
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh", "csh", "tcsh", "fish":
		return true
	default:
		return false
	}
}

func (e DirectExecutor) Execute(ctx context.Context, command Command) ([]byte, error) {
	if e.Start == nil {
		return nil, errors.New("textdelivery: nil command starter")
	}
	if IsShellExecutable(command.Executable) {
		return nil, fmt.Errorf("%w: %q", ErrShellExecutable, command.Executable)
	}
	process := e.Start(ctx, command.Executable, command.Args...)
	if process == nil {
		return nil, errors.New("textdelivery: command starter returned nil process")
	}
	process.SetStdin(bytes.NewReader(command.Payload))
	return process.Output()
}

type osProcess struct{ cmd *exec.Cmd }

func (p *osProcess) SetStdin(r io.Reader)    { p.cmd.Stdin = r }
func (p *osProcess) Output() ([]byte, error) { return p.cmd.Output() }
