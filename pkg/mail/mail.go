package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// FAC-126: the mailbox is a durable, deduplicated, observable delivery
// mechanism, not a best-effort scratch file. Every append is fsync'd under
// a cross-process flock so concurrent writers (including other hosts
// relaying through Redis) never interleave or duplicate records; every
// envelope gets a monotonic per-mailbox Sequence so consumers can track
// exactly how far they've read; malformed records are quarantined instead
// of silently dropped.

const (
	// maxSeenIDs bounds the in-memory dedup set so a long-lived broker never
	// grows unbounded; oldest IDs are evicted FIFO.
	maxSeenIDs = 4096
)

type Envelope struct {
	ID        string    `json:"id"`
	Sequence  int64     `json:"seq"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Read      bool      `json:"read"`
	Timestamp time.Time `json:"timestamp"`
}

// QuarantineEntry records one mailbox line that failed to parse as an
// Envelope. Malformed records are never silently skipped: ReadInbox writes
// one of these to <MailFile>.quarantine.jsonl for every line it can't
// decode, and QuarantineCount surfaces the running total.
type QuarantineEntry struct {
	Line      string    `json:"line"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

type Mailbox struct {
	mu       sync.RWMutex
	MailFile string

	seenMu     sync.Mutex
	seen       map[string]struct{}
	seenOrder  []string
	seenLoaded bool

	quarantineMu sync.Mutex
	quarantined  int

	// lockTimeoutNs, when non-zero, overrides stuckGrace for fail-closed
	// no-progress detection (atomic; safe under concurrent readers).
	// Zero means default stuckGrace. Intended for tests and tight ceilings.
	lockTimeoutNs atomic.Int64
}

func NewMailbox(mailFile string) *Mailbox {
	return &Mailbox{MailFile: mailFile, seen: make(map[string]struct{})}
}

// SetLockTimeout overrides the no-progress stuck bound with a fixed duration.
// d <= 0 restores stuckGrace. Safe for concurrent use (atomic store).
func (m *Mailbox) SetLockTimeout(d time.Duration) {
	if m == nil {
		return
	}
	if d <= 0 {
		m.lockTimeoutNs.Store(0)
		return
	}
	m.lockTimeoutNs.Store(int64(d))
}

// SendMessage appends a freshly-minted envelope to the mailbox.
func (m *Mailbox) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	return m.SendMessageContext(context.Background(), sender, recipient, subject, body)
}

// SendMessageContext is SendMessage with caller-deadline propagation: when
// ctx carries a deadline, lock acquisition fails closed at that deadline
// (typed BLOCKED) instead of using the adaptive wall-clock policy.
//
// If the envelope was durably appended but a post-append unlock/dequeue
// cleanup failed, the envelope is still returned with the cleanup error so
// the caller retains the id (retry of the same id is idempotent).
func (m *Mailbox) SendMessageContext(ctx context.Context, sender, recipient, subject, body string) (*Envelope, error) {
	env := newEnvelope(sender, recipient, subject, body)
	if err := m.appendEnvelopeContext(ctx, env); err != nil {
		// Durable write may have completed before cleanup failed.
		m.mu.Lock()
		seen := m.alreadySeenLocked(env.ID)
		m.mu.Unlock()
		if seen || m.fileHasID(env.ID) {
			return env, err
		}
		return nil, err
	}
	return env, nil
}

// AppendEnvelopeContext appends an envelope whose identity was assigned by a
// durable protocol above the mailbox.  Unlike SendMessageContext this does
// not mint a new ID; retries therefore remain idempotent across restarts.
// The envelope is still assigned a mailbox sequence and is fsync'd before
// this method returns.
func (m *Mailbox) AppendEnvelopeContext(ctx context.Context, env *Envelope) error {
	if env == nil || env.ID == "" {
		return fmt.Errorf("append envelope: stable id is required")
	}
	if env.Recipient == "" || env.Sender == "" {
		return fmt.Errorf("append envelope: sender and recipient are required")
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	return m.appendEnvelopeContext(ctx, env)
}

func newEnvelope(sender, recipient, subject, body string) *Envelope {
	return &Envelope{
		ID:        newID(),
		Sender:    sender,
		Recipient: recipient,
		Subject:   subject,
		Body:      body,
		Read:      false,
		Timestamp: time.Now(),
	}
}

// appendEnvelope reserves the next monotonic sequence number for this
// mailbox file and durably (fsync'd, cross-process flock'd) appends env.
// If env.ID has already been delivered — a redelivered Redis message, or a
// broker's own publish echoing back to itself — the append is skipped so
// redelivery is idempotent: appendEnvelope is the single write path shared
// by SendMessage and the Redis relay subscriber.
func (m *Mailbox) appendEnvelope(env *Envelope) error {
	return m.appendEnvelopeContext(context.Background(), env)
}

func (m *Mailbox) appendEnvelopeContext(ctx context.Context, env *Envelope) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureSeenLoadedLocked()
	if m.alreadySeenLocked(env.ID) {
		if err := m.loadEnvelopeIdentityLocked(env); err != nil {
			return err
		}
		// Idempotent: prior durable append (or same-id retry after a
		// post-append dequeue/cleanup failure) must not re-append.
		return nil
	}

	// Append under the data flock; markSeen immediately after durable
	// appendLine so a subsequent unlock/dequeue/dirsync failure cannot
	// "lose" the envelope id and allow a retry to duplicate the line.
	// Cleanup errors still surface fail-closed from withFileLockContext.
	err := m.withFileLockContext(ctx, func() error {
		// Re-check under the lock: a concurrent writer may have appended
		// the same id between the outer alreadySeen and flock grant.
		if m.alreadySeenLocked(env.ID) {
			return m.loadEnvelopeIdentityLocked(env)
		}
		if m.fileHasID(env.ID) {
			m.markSeenLocked(env.ID)
			if err := m.loadEnvelopeIdentityLocked(env); err != nil {
				return err
			}
			return nil
		}
		seq, err := m.nextSequenceLocked()
		if err != nil {
			return err
		}
		env.Sequence = seq
		data, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("failed to marshal mail envelope: %w", err)
		}
		if err := appendLine(m.MailFile, data); err != nil {
			return err
		}
		// Durable append succeeded: record id before unlock/dequeue so a
		// cleanup failure cannot cause a lost-id → re-append duplicate.
		m.markSeenLocked(env.ID)
		return nil
	})
	if err != nil {
		// Fail-closed on cleanup, but if the id is already durable/seen the
		// caller must treat this as "written with cleanup error" — retry of
		// the same env.ID is a no-op via alreadySeenLocked/fileHasID.
		return err
	}
	return nil
}

func (m *Mailbox) loadEnvelopeIdentityLocked(env *Envelope) error {
	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return err
	}
	for _, line := range splitLines(string(data)) {
		var existing Envelope
		if json.Unmarshal([]byte(line), &existing) == nil && existing.ID == env.ID {
			if existing.Sender != env.Sender || existing.Recipient != env.Recipient || existing.Subject != env.Subject || existing.Body != env.Body {
				return fmt.Errorf("mailbox: envelope ID %q reused with different content", env.ID)
			}
			env.Sequence, env.Timestamp = existing.Sequence, existing.Timestamp
			return nil
		}
	}
	return fmt.Errorf("mailbox: envelope ID %q was seen but is not readable", env.ID)
}

// ensureSeenLoadedLocked seeds the in-memory dedup set from the mailbox
// file's existing contents the first time this Mailbox handle is used, so
// a restarted process doesn't re-relay messages it already durably wrote
// before the crash. Caller must hold m.mu.
func (m *Mailbox) ensureSeenLoadedLocked() {
	m.seenMu.Lock()
	if m.seenLoaded {
		m.seenMu.Unlock()
		return
	}
	m.seenLoaded = true
	m.seenMu.Unlock()

	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return
	}
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(line), &env) == nil && env.ID != "" {
			m.markSeenLocked(env.ID)
		}
	}
}

// alreadySeenLocked reports whether id has already been durably appended.
// The in-memory seen cache is a bounded FIFO window (maxSeenIDs), not the
// authority on dedup — once an ID falls out of that window, a cache miss
// does not mean "never seen". It falls back to the mailbox file itself,
// which retains every envelope ever durably appended, so redelivery of an
// ID older than the cache window still dedupes correctly instead of
// silently writing a duplicate record. Caller must hold m.mu.
func (m *Mailbox) alreadySeenLocked(id string) bool {
	m.seenMu.Lock()
	_, ok := m.seen[id]
	m.seenMu.Unlock()
	if ok {
		return true
	}
	if !m.fileHasID(id) {
		return false
	}
	// Bring it back into the cache so a burst of repeated redelivery for
	// this same old ID doesn't rescan the file every time.
	m.markSeenLocked(id)
	return true
}

// fileHasID scans the durable mailbox file for id. Only reached on a
// seen-cache miss, i.e. a genuine redelivery older than the cache window,
// not the common case.
// ponytail: linear scan over the whole file; add an on-disk index if
// mailbox files start seeing high-volume old-ID redelivery.
func (m *Mailbox) fileHasID(id string) bool {
	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return false
	}
	needle := []byte(`"id":"` + id + `"`)
	return bytes.Contains(data, needle)
}

func (m *Mailbox) markSeenLocked(id string) {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	if _, ok := m.seen[id]; ok {
		return
	}
	m.seen[id] = struct{}{}
	m.seenOrder = append(m.seenOrder, id)
	if len(m.seenOrder) > maxSeenIDs {
		old := m.seenOrder[0]
		m.seenOrder = m.seenOrder[1:]
		delete(m.seen, old)
	}
}

// ReadInbox returns every envelope addressed to recipient (or "all"). Lines
// that fail to parse are quarantined via quarantineLine rather than
// silently dropped, and reading continues past them. If the quarantine sink
// itself fails to durably record one or more malformed lines, ReadInbox
// still returns every successfully-parsed envelope but reports a non-nil
// error: a malformed record that can't even be quarantined is a fail-closed
// condition, not something to swallow the way a normal parse failure is.
func (m *Mailbox) ReadInbox(recipient string) ([]*Envelope, error) {
	return m.ReadInboxContext(context.Background(), recipient)
}

// ReadInboxContext is ReadInbox with deadline inheritance for quarantine
// lock acquisition (same fail-closed quarantine semantics).
func (m *Mailbox) ReadInboxContext(ctx context.Context, recipient string) ([]*Envelope, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, err := os.Stat(m.MailFile); os.IsNotExist(err) {
		return nil, nil
	}

	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read mail file: %w", err)
	}

	var res []*Envelope
	var quarantineErrs []error

	rawLines := splitLines(string(data))
	for _, l := range rawLines {
		if len(l) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			if qErr := m.quarantineLineContext(ctx, l, err); qErr != nil {
				quarantineErrs = append(quarantineErrs, qErr)
			}
			continue
		}
		if recipient == "" || env.Recipient == recipient || env.Recipient == "all" {
			res = append(res, &env)
		}
	}

	if len(quarantineErrs) > 0 {
		return res, fmt.Errorf("failed to durably quarantine %d malformed record(s): %w", len(quarantineErrs), errors.Join(quarantineErrs...))
	}
	return res, nil
}

// quarantineLine durably records a line ReadInbox could not parse and bumps
// the surfaced counter, returning an error if the durable write itself
// failed so the caller can fail closed instead of believing the record was
// preserved when it wasn't.
func (m *Mailbox) quarantineLine(line string, cause error) error {
	return m.quarantineLineContext(context.Background(), line, cause)
}

func (m *Mailbox) quarantineLineContext(ctx context.Context, line string, cause error) error {
	m.quarantineMu.Lock()
	m.quarantined++
	m.quarantineMu.Unlock()

	entry := QuarantineEntry{Line: line, Reason: cause.Error(), Timestamp: time.Now()}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal quarantine entry: %w", err)
	}
	return m.withFileLockContext(ctx, func() error {
		return appendLine(m.MailFile+".quarantine.jsonl", data)
	})
}

// QuarantineCount returns the number of malformed records surfaced since
// this Mailbox handle was constructed.
func (m *Mailbox) QuarantineCount() int {
	m.quarantineMu.Lock()
	defer m.quarantineMu.Unlock()
	return m.quarantined
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

var idCounter atomic.Int64

func init() {
	idCounter.Store(time.Now().UnixMilli())
}

func newID() string {
	n := idCounter.Add(1)
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	// Include PID so concurrent processes cannot mint colliding IDs (the
	// per-process counter alone restarts from the same UnixMilli seed).
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), n)
}

// nextSequenceLocked reserves and durably persists the next monotonic
// sequence number for this mailbox file. Caller must hold the file lock.
// The counter is reserved (written to disk) before the caller uses it, so a
// crash between reservation and the envelope append can only ever leave a
// gap in the sequence, never a duplicate.
func (m *Mailbox) nextSequenceLocked() (int64, error) {
	seqPath := m.MailFile + ".seq"

	var cur int64
	data, err := os.ReadFile(seqPath)
	switch {
	case err == nil:
		cur, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("corrupt sequence file %s: %w", seqPath, err)
		}
	case os.IsNotExist(err):
		// first message in this mailbox
	default:
		return 0, fmt.Errorf("failed to read sequence file: %w", err)
	}

	next := cur + 1
	if err := writeFileAtomic(seqPath, []byte(strconv.FormatInt(next, 10)), 0644); err != nil {
		return 0, fmt.Errorf("failed to reserve sequence: %w", err)
	}
	return next, nil
}

// appendLine appends data plus a trailing newline to path, fsync'ing before
// returning so the write survives a crash immediately after this call, then
// fsyncs the containing directory too — the directory entry created by the
// first write to a new file is separate metadata from the file's own data
// and needs its own durability barrier, or a crash can leave the fsync'd
// data on disk but unreachable because the directory forgot it exists.
func appendLine(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open mail artifact: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("failed to write mail artifact: %w", err)
	}
	if err := fileSyncFn(f); err != nil {
		f.Close()
		return fmt.Errorf("failed to fsync mail artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close mail artifact: %w", err)
	}
	return syncDirFn(path)
}

// writeFileAtomic writes data to a temp file, fsyncs it, then renames it
// over path — so a reader never observes a torn write and a crash mid-write
// leaves the previous, valid contents in place — then fsyncs the containing
// directory so the rename itself (which repoints the directory entry) is
// crash-durable too, not just the file content it points at.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicFn(path, data, perm)
}

// writeFileAtomicFn is the production implementation; tests may override to
// inject a post-dead-letter state-save failure without chmod tricks that
// also break the ticket queue.
var writeFileAtomicFn = writeFileAtomicImpl

func writeFileAtomicImpl(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := fileSyncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDirFn(path)
}

// fileSyncFn / syncDirFn are overridable hooks for mutation probes that
// prove fsync is invoked (count calls) or must fail closed (inject error).
// Production uses real *os.File.Sync and directory Sync.
var (
	fileSyncFn = func(f *os.File) error { return f.Sync() }
	syncDirFn  = syncDir
)

// syncDir fsyncs the directory containing path. Opening a directory and
// calling Sync on it is the standard way to durably commit directory-entry
// metadata (file creation, rename) on the platforms this package supports.
func syncDir(path string) error {
	dir := filepath.Dir(path)
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("failed to open directory for fsync: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to fsync directory: %w", err)
	}
	return nil
}

// RedisClient defines the Redis surface we consume.
type RedisClient interface {
	Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd
	PSubscribe(ctx context.Context, patterns ...string) Subscription
	Close() error
}

// Subscription is the consumer side of a Redis pub/sub channel.
type Subscription interface {
	Channel(opts ...redis.ChannelOption) <-chan *redis.Message
	Close() error
}

// MessageBrokerOption configures a MessageBroker.
type MessageBrokerOption func(*MessageBroker)

func WithRedis(client RedisClient, channelPrefix string) MessageBrokerOption {
	return func(b *MessageBroker) {
		b.redis = client
		b.channelPrefix = channelPrefix
	}
}

// MessageBroker wraps a local Mailbox with optional Redis pub/sub syncing.
type MessageBroker struct {
	mb             *Mailbox
	redis          RedisClient
	channelPrefix  string
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	errCh          chan error
	droppedErrs    atomic.Int64
	persistFails   atomic.Int64
	lastPersistErr atomic.Value // holds error; see PersistenceFailed
}

func NewMessageBroker(mailbox *Mailbox, opts ...MessageBrokerOption) *MessageBroker {
	ctx, cancel := context.WithCancel(context.Background())
	b := &MessageBroker{
		mb:     mailbox,
		ctx:    ctx,
		cancel: cancel,
		errCh:  make(chan error, 16),
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.redis != nil {
		// Subscribe before replaying the outbox: an entry that never made it
		// to the local mailbox before the last crash (see SendMessage) can
		// only be recovered via this broker's own self-echo, so the pattern
		// subscription must already be registered before FlushOutbox
		// publishes it — otherwise the replay publish has no listener and
		// the message is lost a second time.
		b.startSubscriber()
		// A restarted process is exactly when outbox replay matters most:
		// any envelope durably appended locally before the last crash but
		// never confirmed published gets another chance here.
		if _, err := b.FlushOutbox(); err != nil {
			b.reportErr(fmt.Errorf("startup outbox replay failed: %w", err))
		}
	}
	return b
}

func (b *MessageBroker) Close() error {
	b.cancel()
	b.wg.Wait()
	if b.redis != nil {
		return b.redis.Close()
	}
	return nil
}

// Errs surfaces publish and subscribe failures that SendMessage's return
// value can't carry asynchronously (e.g. the subscriber goroutine's channel
// closing). Buffered and best-effort: once full, an error is never silently
// discarded — it's durably appended to <MailFile>.errors.jsonl instead (see
// reportErr), and DroppedErrCount reports how many took that path.
func (b *MessageBroker) Errs() <-chan error {
	return b.errCh
}

// DroppedErrCount returns how many errors overflowed Errs() and were
// durably retained on disk instead of delivered live.
func (b *MessageBroker) DroppedErrCount() int64 {
	return b.droppedErrs.Load()
}

func (b *MessageBroker) reportErr(err error) {
	if err == nil {
		return
	}
	select {
	case b.errCh <- err:
	default:
		// Errs() isn't being drained fast enough. Never drop this silently:
		// retain it durably so nothing is lost just because nobody was
		// listening at the moment it happened.
		b.persistDroppedErr(err)
	}
}

// persistDroppedErr durably appends an error that overflowed errCh to
// <MailFile>.errors.jsonl. There's no further fallback if the filesystem
// itself is failing, but that failure is never swallowed: it's recorded in
// a durable, observable fail-closed health state (see PersistenceFailed) so
// a caller can tell "the error was retained on disk" apart from "the error
// and its retention record are both gone" instead of silently assuming the
// former.
func (b *MessageBroker) persistDroppedErr(err error) {
	b.droppedErrs.Add(1)
	entry := struct {
		Error     string    `json:"error"`
		Timestamp time.Time `json:"timestamp"`
	}{Error: err.Error(), Timestamp: time.Now()}
	data, mErr := json.Marshal(entry)
	if mErr != nil {
		b.recordPersistFailure(mErr)
		return
	}
	writeErr := b.mb.withFileLockContext(b.ctx, func() error {
		return appendLine(b.mb.MailFile+".errors.jsonl", data)
	})
	if writeErr != nil {
		b.recordPersistFailure(writeErr)
	}
}

func (b *MessageBroker) recordPersistFailure(err error) {
	b.persistFails.Add(1)
	b.lastPersistErr.Store(persistErrHolder{err: err})
}

// persistErrHolder lets lastPersistErr be stored in an atomic.Value: the
// stored type must be consistent and non-nil, so a bare error interface
// (which can be nil) doesn't work directly.
type persistErrHolder struct{ err error }

// PersistenceFailed reports whether an error dropped by Errs() has ever
// failed to be durably retained on disk — the fail-closed signal that
// error data has actually been lost, not just delayed.
func (b *MessageBroker) PersistenceFailed() bool {
	return b.persistFails.Load() > 0
}

// PersistenceFailureCount returns how many times persistDroppedErr's own
// durable write has failed.
func (b *MessageBroker) PersistenceFailureCount() int64 {
	return b.persistFails.Load()
}

// LastPersistenceError returns the most recent error encountered while
// trying to durably retain a dropped error, or nil if persistence has never
// failed.
func (b *MessageBroker) LastPersistenceError() error {
	h, _ := b.lastPersistErr.Load().(persistErrHolder)
	return h.err
}

// SendMessage records a durable outbox entry for the Redis fan-out BEFORE
// the local mailbox append, and the local append BEFORE attempting to
// publish. This ordering closes the crash window a mailbox-first write left
// open: if mailbox append happened first and a crash landed before the
// outbox entry was written, the message was visible locally but its record
// of needing Redis fan-out was gone forever — no restart could recover it.
// With the outbox written first, a crash between the two durable writes
// still leaves a durable outbox entry, and this broker's own PSubscribe
// pattern includes its own publishes: FlushOutbox (retried automatically on
// the next NewMessageBroker, i.e. after a restart) republishes it, the
// self-echo lands back on b.mb.appendEnvelope, and the message is
// recovered into the local mailbox — idempotently, since every subscriber
// dedupes by envelope ID. A replay that reaches Redis a second time for an
// already-published message is likewise harmless, so at-least-once outbox
// delivery plus an idempotent consumer gives an effectively-once result
// without needing a Redis-side ack (plain pub/sub has none).
func (b *MessageBroker) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	return b.SendMessageContext(b.ctx, sender, recipient, subject, body)
}

// SendMessageContext is SendMessage with caller-deadline inheritance on
// outbox mutation and local mailbox append (both take the mailbox flock).
func (b *MessageBroker) SendMessageContext(ctx context.Context, sender, recipient, subject, body string) (*Envelope, error) {
	if ctx == nil {
		ctx = b.ctx
	}
	if b.redis == nil {
		return b.mb.SendMessageContext(ctx, sender, recipient, subject, body)
	}

	env := newEnvelope(sender, recipient, subject, body)
	channel := b.channelPrefix + "." + recipient

	if err := b.addOutboxEntryContext(ctx, env, channel); err != nil {
		return nil, fmt.Errorf("failed to durably record outbox entry for %s: %w", env.ID, err)
	}

	if err := b.mb.appendEnvelopeContext(ctx, env); err != nil {
		// The outbox entry is already durable, so the message isn't lost —
		// FlushOutbox (or this broker's own subscription self-echo once
		// publish succeeds) will still deliver it into the local mailbox.
		// Still surface the immediate local-append failure to the caller.
		b.reportErr(fmt.Errorf("local mailbox append failed for %s (outbox retains it): %w", env.ID, err))
		return env, err
	}

	if err := b.publishOne(env, channel); err != nil {
		b.reportErr(err)
		return env, err // still durably queued in the outbox; FlushOutbox will retry it
	}
	if err := b.removeOutboxEntryContext(ctx, env.ID); err != nil {
		// Published successfully but couldn't clear the record: harmless —
		// the next flush just re-publishes a message receivers already
		// dedupe by ID.
		b.reportErr(fmt.Errorf("outbox cleanup failed for %s: %w", env.ID, err))
	}
	return env, nil
}

// SendEnvelopeContext delivers a caller-identified envelope.  It mirrors
// SendMessageContext's outbox-before-mailbox-before-publish ordering while
// preserving the caller's stable ID for protocol-level deduplication.
func (b *MessageBroker) SendEnvelopeContext(ctx context.Context, env *Envelope) error {
	if env == nil || env.ID == "" {
		return fmt.Errorf("send envelope: stable id is required")
	}
	if env.Sender == "" || env.Recipient == "" {
		return fmt.Errorf("send envelope: sender and recipient are required")
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}
	if ctx == nil {
		ctx = b.ctx
	}
	if b.redis == nil {
		return b.mb.appendEnvelopeContext(ctx, env)
	}
	channel := b.channelPrefix + "." + env.Recipient
	if err := b.addOutboxEntryContext(ctx, env, channel); err != nil {
		return fmt.Errorf("failed to durably record outbox entry for %s: %w", env.ID, err)
	}
	if err := b.mb.appendEnvelopeContext(ctx, env); err != nil {
		b.reportErr(fmt.Errorf("local mailbox append failed for %s (outbox retains it): %w", env.ID, err))
		return err
	}
	if err := b.publishOne(env, channel); err != nil {
		b.reportErr(err)
		return err
	}
	if err := b.removeOutboxEntryContext(ctx, env.ID); err != nil {
		b.reportErr(fmt.Errorf("outbox cleanup failed for %s: %w", env.ID, err))
	}
	return nil
}

func (b *MessageBroker) publishOne(env *Envelope, channel string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope for publish: %w", err)
	}
	if err := b.redis.Publish(b.ctx, channel, data).Err(); err != nil {
		return fmt.Errorf("redis publish to %s failed: %w", channel, err)
	}
	return nil
}

// outboxEntry is one not-yet-confirmed-published envelope, persisted to
// <MailFile>.outbox.json before the publish attempt.
type outboxEntry struct {
	Envelope Envelope `json:"envelope"`
	Channel  string   `json:"channel"`
}

func (b *MessageBroker) outboxPath() string {
	return b.mb.MailFile + ".outbox.json"
}

func (b *MessageBroker) loadOutboxUnlocked() (map[string]outboxEntry, error) {
	data, err := os.ReadFile(b.outboxPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]outboxEntry{}, nil
		}
		return nil, fmt.Errorf("failed to read outbox: %w", err)
	}
	var entries map[string]outboxEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Redact absolute outbox path from error surface.
		return nil, fmt.Errorf("corrupt outbox file %s: %w", filepath.Base(b.outboxPath()), err)
	}
	if entries == nil {
		entries = map[string]outboxEntry{}
	}
	return entries, nil
}

func (b *MessageBroker) mutateOutboxLocked(fn func(map[string]outboxEntry) error) error {
	return b.mutateOutboxLockedContext(context.Background(), fn)
}

func (b *MessageBroker) mutateOutboxLockedContext(ctx context.Context, fn func(map[string]outboxEntry) error) error {
	return b.mb.withFileLockContext(ctx, func() error {
		entries, err := b.loadOutboxUnlocked()
		if err != nil {
			return err
		}
		if err := fn(entries); err != nil {
			return err
		}
		data, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("failed to marshal outbox: %w", err)
		}
		return writeFileAtomic(b.outboxPath(), data, 0644)
	})
}

func (b *MessageBroker) addOutboxEntry(env *Envelope, channel string) error {
	return b.addOutboxEntryContext(context.Background(), env, channel)
}

func (b *MessageBroker) addOutboxEntryContext(ctx context.Context, env *Envelope, channel string) error {
	return b.mutateOutboxLockedContext(ctx, func(entries map[string]outboxEntry) error {
		entries[env.ID] = outboxEntry{Envelope: *env, Channel: channel}
		return nil
	})
}

func (b *MessageBroker) removeOutboxEntry(id string) error {
	return b.removeOutboxEntryContext(context.Background(), id)
}

func (b *MessageBroker) removeOutboxEntryContext(ctx context.Context, id string) error {
	return b.mutateOutboxLockedContext(ctx, func(entries map[string]outboxEntry) error {
		delete(entries, id)
		return nil
	})
}

func (b *MessageBroker) snapshotOutbox() (map[string]outboxEntry, error) {
	return b.snapshotOutboxContext(b.ctx)
}

func (b *MessageBroker) snapshotOutboxContext(ctx context.Context) (map[string]outboxEntry, error) {
	var entries map[string]outboxEntry
	err := b.mb.withFileLockContext(ctx, func() error {
		var err error
		entries, err = b.loadOutboxUnlocked()
		return err
	})
	return entries, err
}

// FlushOutbox retries publishing every entry still recorded in the durable
// outbox — messages whose local append succeeded but whose Redis publish
// never confirmed. Safe to call any time, including right after
// NewMessageBroker on a freshly restarted process (where it's called
// automatically).
func (b *MessageBroker) FlushOutbox() (published int, err error) {
	return b.FlushOutboxContext(b.ctx)
}

// FlushOutboxContext is FlushOutbox with deadline inheritance on outbox
// snapshot/remove lock acquisitions.
func (b *MessageBroker) FlushOutboxContext(ctx context.Context) (published int, err error) {
	if b.redis == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = b.ctx
	}
	entries, err := b.snapshotOutboxContext(ctx)
	if err != nil {
		return 0, err
	}
	for id, entry := range entries {
		env := entry.Envelope
		if pubErr := b.publishOne(&env, entry.Channel); pubErr != nil {
			b.reportErr(fmt.Errorf("outbox replay publish failed for %s: %w", id, pubErr))
			continue
		}
		if err := b.removeOutboxEntryContext(ctx, id); err != nil {
			return published, err
		}
		published++
	}
	return published, nil
}

func (b *MessageBroker) ReadInbox(recipient string) ([]*Envelope, error) {
	return b.ReadInboxContext(b.ctx, recipient)
}

// ReadInboxContext is ReadInbox with deadline inheritance for quarantine
// paths inside the local mailbox.
func (b *MessageBroker) ReadInboxContext(ctx context.Context, recipient string) ([]*Envelope, error) {
	return b.mb.ReadInboxContext(ctx, recipient)
}

// startSubscriber relays messages from other hosts into the local mailbox
// via Mailbox.appendEnvelope, the same durable, deduplicating write path
// SendMessage uses — so a broker's own publish echoing back through its
// pattern subscription is a no-op, not a duplicate, and a message relayed
// twice (Redis redelivery) is idempotent. PSubscribe is called synchronously
// here, before startSubscriber returns, so a caller that calls FlushOutbox
// immediately afterward (NewMessageBroker's startup replay) is guaranteed
// the pattern is already registered and won't publish into the void.
func (b *MessageBroker) startSubscriber() {
	pattern := b.channelPrefix + ".*"
	sub := b.redis.PSubscribe(b.ctx, pattern)

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-b.ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					b.reportErr(fmt.Errorf("redis subscription channel closed for pattern %s", pattern))
					return
				}
				var env Envelope
				if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
					b.reportErr(fmt.Errorf("discarding malformed redis payload on %s: %w", msg.Channel, err))
					continue
				}
				if err := b.mb.appendEnvelopeContext(b.ctx, &env); err != nil {
					b.reportErr(fmt.Errorf("failed to relay redis message %s into local mailbox: %w", env.ID, err))
				}
			}
		}
	}()
}
