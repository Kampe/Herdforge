package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// lockTimeout bounds how long a writer waits for the cross-process file
	// lock before failing closed.
	lockTimeout = 5 * time.Second
	// lockPollInterval is how often a blocked writer retries the lock.
	lockPollInterval = 10 * time.Millisecond
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
}

func NewMailbox(mailFile string) *Mailbox {
	return &Mailbox{MailFile: mailFile, seen: make(map[string]struct{})}
}

// SendMessage appends a freshly-minted envelope to the mailbox.
func (m *Mailbox) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	env := &Envelope{
		ID:        newID(),
		Sender:    sender,
		Recipient: recipient,
		Subject:   subject,
		Body:      body,
		Read:      false,
		Timestamp: time.Now(),
	}
	if err := m.appendEnvelope(env); err != nil {
		return nil, err
	}
	return env, nil
}

// appendEnvelope reserves the next monotonic sequence number for this
// mailbox file and durably (fsync'd, cross-process flock'd) appends env.
// If env.ID has already been delivered — a redelivered Redis message, or a
// broker's own publish echoing back to itself — the append is skipped so
// redelivery is idempotent: appendEnvelope is the single write path shared
// by SendMessage and the Redis relay subscriber.
func (m *Mailbox) appendEnvelope(env *Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ensureSeenLoadedLocked()
	if m.alreadySeenLocked(env.ID) {
		return nil
	}

	err := m.withFileLock(func() error {
		seq, err := m.nextSequenceLocked()
		if err != nil {
			return err
		}
		env.Sequence = seq
		data, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("failed to marshal mail envelope: %w", err)
		}
		return appendLine(m.MailFile, data)
	})
	if err != nil {
		return err
	}
	m.markSeenLocked(env.ID)
	return nil
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

func (m *Mailbox) alreadySeenLocked(id string) bool {
	m.seenMu.Lock()
	defer m.seenMu.Unlock()
	_, ok := m.seen[id]
	return ok
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
// silently dropped, and reading continues past them.
func (m *Mailbox) ReadInbox(recipient string) ([]*Envelope, error) {
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

	rawLines := splitLines(string(data))
	for _, l := range rawLines {
		if len(l) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			m.quarantineLine(l, err)
			continue
		}
		if recipient == "" || env.Recipient == recipient || env.Recipient == "all" {
			res = append(res, &env)
		}
	}

	return res, nil
}

// quarantineLine durably records a line ReadInbox could not parse and bumps
// the surfaced counter. Best-effort: a failure to write the quarantine file
// does not fail the read (ReadInbox's error contract is unchanged), but the
// in-memory counter still advances so Stats() reflects the loss.
func (m *Mailbox) quarantineLine(line string, cause error) {
	m.quarantineMu.Lock()
	m.quarantined++
	m.quarantineMu.Unlock()

	entry := QuarantineEntry{Line: line, Reason: cause.Error(), Timestamp: time.Now()}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = m.withFileLock(func() error {
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
	return fmt.Sprintf("%s-%d", host, n)
}

// withFileLock runs fn while holding an exclusive, cross-process advisory
// lock on <MailFile>.lock. It fails closed: if the lock cannot be acquired
// within lockTimeout, it returns an error instead of proceeding unlocked, so
// concurrent multi-process writers can never interleave or corrupt records.
func (m *Mailbox) withFileLock(fn func() error) error {
	dir := filepath.Dir(m.MailFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create mail directory: %w", err)
	}

	lockPath := m.MailFile + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open mailbox lock file: %w", err)
	}
	defer f.Close()

	fd := int(f.Fd())
	deadline := time.Now().Add(lockTimeout)
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mailbox lock %s: timed out after %s", lockPath, lockTimeout)
		}
		time.Sleep(lockPollInterval)
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)

	return fn()
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
// returning so the write survives a crash immediately after this call.
func appendLine(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", path, err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("failed to fsync %s: %w", path, err)
	}
	return f.Close()
}

// writeFileAtomic writes data to a temp file, fsyncs it, then renames it
// over path — so a reader never observes a torn write and a crash mid-write
// leaves the previous, valid contents in place.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	mb            *Mailbox
	redis         RedisClient
	channelPrefix string
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	errCh         chan error
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
		b.startSubscriber()
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
// closing). Buffered and best-effort: once full, further errors are dropped
// rather than blocking message delivery — read it if you need visibility
// beyond SendMessage's own returned error.
func (b *MessageBroker) Errs() <-chan error {
	return b.errCh
}

func (b *MessageBroker) reportErr(err error) {
	if err == nil {
		return
	}
	select {
	case b.errCh <- err:
	default:
	}
}

func (b *MessageBroker) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	env, err := b.mb.SendMessage(sender, recipient, subject, body)
	if err != nil {
		return nil, err
	}
	if b.redis != nil {
		channel := b.channelPrefix + "." + recipient
		data, err := json.Marshal(env)
		if err != nil {
			return env, fmt.Errorf("failed to marshal envelope for publish: %w", err)
		}
		if err := b.redis.Publish(b.ctx, channel, data).Err(); err != nil {
			pubErr := fmt.Errorf("redis publish to %s failed: %w", channel, err)
			b.reportErr(pubErr)
			return env, pubErr
		}
	}
	return env, nil
}

func (b *MessageBroker) ReadInbox(recipient string) ([]*Envelope, error) {
	return b.mb.ReadInbox(recipient)
}

// startSubscriber relays messages from other hosts into the local mailbox
// via Mailbox.appendEnvelope, the same durable, deduplicating write path
// SendMessage uses — so a broker's own publish echoing back through its
// pattern subscription is a no-op, not a duplicate, and a message relayed
// twice (Redis redelivery) is idempotent.
func (b *MessageBroker) startSubscriber() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		pattern := b.channelPrefix + ".*"
		sub := b.redis.PSubscribe(b.ctx, pattern)
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
				if err := b.mb.appendEnvelope(&env); err != nil {
					b.reportErr(fmt.Errorf("failed to relay redis message %s into local mailbox: %w", env.ID, err))
				}
			}
		}
	}()
}
