package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// FAC-162 lock fairness: cross-process ticket queue with causal head-of-line
// handoff. Fairness is the ordered queue, not a longer wall-clock timeout.
//
// Protocol:
//  1. enqueueWaiter issues a monotonic ticket via an atomically replaced
//     sequence file (writeFileAtomic) under a separate meta flock, then
//     O_EXCL-creates the final token name bound to pid+start_ns(+boot).
//     Staging debris left by crashed publishers is reaped under qmeta.
//  2. waitForTurn blocks until this ticket is the minimum live ticket.
//     Waiters themselves detect+reap a dead head (under qmeta) so progress
//     does not require a new enqueue. Non-heads never flock the data lock.
//     Mutation seam: skipWaitForTurn (tests only) disables this gate.
//  3. Head acquires the data flock; only EWOULDBLOCK/EAGAIN are retried.
//     Expired/cancelled contexts never enter the critical section, even
//     when the lock is free (fail-closed deadline).
//  4. On release: clear owner, unlock data, dequeue token (remove+dirsync);
//     unlock/close/dequeue failures are always joined (never discarded).
//
// Stuck detection is progress-based (stuckGrace). Context deadlines inherit.

const (
	stuckGrace       = 5 * time.Second
	maxLockWait      = 2 * time.Minute
	lockPollInterval = 10 * time.Millisecond
	diagLockBound    = 200 * time.Millisecond
)

// ErrMailboxLockTimeout is the typed sentinel for fail-closed lock timeouts.
var ErrMailboxLockTimeout = errors.New("BLOCKED(mailbox_lock_timeout)")

// LockTimeoutError is a typed BLOCKED failure. MailboxID is basename-only.
type LockTimeoutError struct {
	MailboxID  string
	Waited     time.Duration
	Bound      time.Duration
	QueueDepth int
	Position   int
	OwnerPID   int
	SelfPID    int
	Reason     string
}

func (e *LockTimeoutError) Error() string {
	if e == nil {
		return ErrMailboxLockTimeout.Error()
	}
	id := e.MailboxID
	if id == "" {
		id = "mailbox"
	}
	return fmt.Sprintf(
		"BLOCKED(mailbox_lock_timeout): mailbox=%s waited=%s bound=%s queue_depth=%d position=%d owner_pid=%d reason=%s",
		id, e.Waited, e.Bound, e.QueueDepth, e.Position, e.OwnerPID, e.Reason,
	)
}

func (e *LockTimeoutError) Unwrap() error { return ErrMailboxLockTimeout }
func (e *LockTimeoutError) Is(target error) bool {
	return target == ErrMailboxLockTimeout
}

// LockDiag is durable lock-timeout evidence (basename identity only).
type LockDiag struct {
	Status     string    `json:"status"`
	Timestamp  time.Time `json:"timestamp"`
	MailboxID  string    `json:"mailbox_id"`
	WaitedMs   int64     `json:"waited_ms"`
	BoundMs    int64     `json:"bound_ms"`
	QueueDepth int       `json:"queue_depth"`
	Position   int       `json:"position"`
	OwnerPID   int       `json:"owner_pid,omitempty"`
	SelfPID    int       `json:"self_pid"`
	Reason     string    `json:"reason"`
}

// clockHooks are race-safe test injectors.
type clockHooks struct {
	Now   func() time.Time
	Sleep func(time.Duration)
	Flock func(fd int, how int) error
}

var hooks atomic.Pointer[clockHooks]

// skipWaitForTurn is a mutation seam: when true, withFileLockContext skips
// waitForTurn so non-heads can barge into the data flock. Production stays
// false; tests toggle it to prove fairness is causal.
var skipWaitForTurn atomic.Bool

// removeTokenFn / tokenDirSyncFn are mutation seams for cleanup durability.
var (
	removeTokenFn = os.Remove
	// tokenDirSyncFn defaults to the current syncDirFn so dirsync inject
	// still applies unless a test overrides this seam specifically.
	tokenDirSyncFn = func(path string) error { return syncDirFn(path) }
)

func setTestHooks(h *clockHooks) { hooks.Store(h) }
func clearTestHooks()            { hooks.Store(nil) }

func hookNow() time.Time {
	if h := hooks.Load(); h != nil && h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func hookSleep(d time.Duration) {
	if h := hooks.Load(); h != nil && h.Sleep != nil {
		h.Sleep(d)
		return
	}
	time.Sleep(d)
}

func hookFlock(fd int, how int) error {
	if h := hooks.Load(); h != nil && h.Flock != nil {
		return h.Flock(fd, how)
	}
	return syscall.Flock(fd, how)
}

func (m *Mailbox) withFileLock(fn func() error) error {
	return m.withFileLockContext(context.Background(), fn)
}

func (m *Mailbox) withFileLockContext(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil {
		return errors.New("mailbox lock: nil mailbox")
	}
	dir := filepath.Dir(m.MailFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return redactErr(fmt.Errorf("failed to create mail directory: %w", err))
	}

	mailboxID := mailboxIdentity(m.MailFile)
	lockPath := m.MailFile + ".lock"
	start := hookNow()
	bound, reason := m.lockWaitBound(ctx)

	// Fail-closed before any queue mutation: an already-expired/cancelled
	// context must never enqueue or enter the critical section, even when
	// the data lock is completely free (uncontended).
	if err := m.checkWaitLimits(ctx, start, start, bound, reason, lockPath, mailboxID, 0, 0); err != nil {
		return redactErr(err)
	}

	// Fail-closed: enqueueWaiter errors never silently degrade to unordered
	// data-flock-only. Corrupt counters, fsync/dirsync/token failures, and
	// permission errors all surface as hard errors so FIFO/durability cannot
	// be bypassed by discarding qerr.
	ticket, tokenPath, _, qerr := m.enqueueWaiter()
	if qerr != nil {
		return redactErr(fmt.Errorf("mailbox lock enqueue failed for %s: %w", mailboxID, qerr))
	}

	finishQueue := func(err error) error {
		dequeueErr := m.dequeueWaiter(tokenPath)
		return redactErr(errors.Join(err, dequeueErr))
	}

	if !skipWaitForTurn.Load() {
		if err := m.waitForTurn(ctx, ticket, tokenPath, start, bound, reason, mailboxID); err != nil {
			return finishQueue(err)
		}
	}

	// Re-check deadline after becoming head: waitForTurn returns immediately
	// when already head, so an expired ctx must still fail closed here before
	// any uncontended flock + mutate.
	if err := m.checkWaitLimits(ctx, start, hookNow(), bound, reason, lockPath, mailboxID, 1, 1); err != nil {
		return finishQueue(err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return finishQueue(fmt.Errorf("failed to open mailbox lock for %s: %w", mailboxID, err))
	}

	fd := int(f.Fd())
	lastProgress := hookNow()
	lastOwner := readLockOwnerPID(lockPath)
	for {
		err := hookFlock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			closeErr := f.Close()
			return finishQueue(errors.Join(
				fmt.Errorf("mailbox lock acquire failed for %s: %w", mailboxID, err),
				closeErr,
			))
		}
		// Contended acquire: if HOL waiter looks dead, try non-blocking reap.
		if m.headTokenDead() {
			if rerr := m.reapDeadWaitersUnderMeta(); rerr != nil {
				closeErr := f.Close()
				return finishQueue(errors.Join(
					fmt.Errorf("mailbox lock dead-head reap failed for %s: %w", mailboxID, rerr),
					closeErr,
				))
			}
		}
		head, pos, depthNow, herr := m.queuePosition(ticket)
		if herr != nil {
			closeErr := f.Close()
			return finishQueue(errors.Join(
				fmt.Errorf("mailbox lock queue inspect failed for %s: %w", mailboxID, herr),
				closeErr,
			))
		}
		if !skipWaitForTurn.Load() && !head {
			closeErr := f.Close()
			return finishQueue(errors.Join(
				m.newLockTimeout(lockPath, mailboxID, hookNow().Sub(start), bound, depthNow, pos, "lost_head_while_acquiring"),
				closeErr,
			))
		}
		owner := readLockOwnerPID(lockPath)
		if owner != lastOwner {
			lastOwner = owner
			lastProgress = hookNow()
		}
		if err := m.checkWaitLimits(ctx, start, lastProgress, bound, reason, lockPath, mailboxID, depthNow, pos); err != nil {
			closeErr := f.Close()
			return finishQueue(errors.Join(err, closeErr))
		}
		hookSleep(lockPollInterval)
	}

	// Final deadline gate after exclusive flock, before any mutation.
	if err := m.checkWaitLimits(ctx, start, hookNow(), bound, reason, lockPath, mailboxID, 1, 1); err != nil {
		unlockErr := hookFlock(fd, syscall.LOCK_UN)
		closeErr := f.Close()
		return finishQueue(errors.Join(err, unlockErr, closeErr))
	}

	if werr := writeLockOwnerPID(f); werr != nil {
		unlockErr := hookFlock(fd, syscall.LOCK_UN)
		closeErr := f.Close()
		return finishQueue(errors.Join(
			fmt.Errorf("mailbox lock owner record failed for %s: %w", mailboxID, werr),
			unlockErr,
			closeErr,
		))
	}

	fnErr := fn()
	clearErr := clearLockOwnerPID(f)
	unlockErr := hookFlock(fd, syscall.LOCK_UN)
	closeErr := f.Close()
	dequeueErr := m.dequeueWaiter(tokenPath)
	return redactErr(joinLockReleaseErrors(fnErr, clearErr, unlockErr, closeErr, dequeueErr, mailboxID))
}

func joinLockReleaseErrors(fnErr, clearErr, unlockErr, closeErr, dequeueErr error, mailboxID string) error {
	var parts []error
	if fnErr != nil {
		parts = append(parts, fnErr)
	}
	if clearErr != nil {
		parts = append(parts, fmt.Errorf("mailbox lock owner clear failed for %s: %w", mailboxID, clearErr))
	}
	if unlockErr != nil {
		parts = append(parts, fmt.Errorf("mailbox lock unlock failed for %s: %w", mailboxID, unlockErr))
	}
	if closeErr != nil {
		parts = append(parts, fmt.Errorf("mailbox lock close failed for %s: %w", mailboxID, closeErr))
	}
	if dequeueErr != nil {
		parts = append(parts, fmt.Errorf("mailbox lock dequeue failed for %s: %w", mailboxID, dequeueErr))
	}
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		return errors.Join(parts...)
	}
}

func isLockBusy(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func mailboxIdentity(mailFile string) string {
	base := filepath.Base(mailFile)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "mailbox"
	}
	if strings.Contains(base, string(filepath.Separator)) {
		base = filepath.Base(base)
	}
	return base
}

func (m *Mailbox) lockWaitBound(ctx context.Context) (time.Duration, string) {
	if m != nil {
		if ns := m.lockTimeoutNs.Load(); ns > 0 {
			return time.Duration(ns), "fixed_override"
		}
	}
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok {
			remain := dl.Sub(hookNow())
			if remain < 0 {
				remain = 0
			}
			if remain > maxLockWait {
				return maxLockWait, "context_deadline_capped"
			}
			return remain, "context_deadline"
		}
	}
	return stuckGrace, "stuck_grace"
}

func (m *Mailbox) ticketPath() string { return m.MailFile + ".lock.ticket" }
func (m *Mailbox) qmetaPath() string  { return m.MailFile + ".lock.qmeta" }
func (m *Mailbox) waiterDir() string  { return m.MailFile + ".lock.waiters" }

// enqueueWaiter issues a crash-safe monotonic ticket and O_EXCL token.
// Counter lives in .lock.ticket (atomic rename+dirsync); meta flock is a
// separate inode (.lock.qmeta) so truncate-crash cannot reset tickets.
func (m *Mailbox) enqueueWaiter() (ticket int64, tokenPath string, depth int, err error) {
	qdir := m.waiterDir()
	if err := os.MkdirAll(qdir, 0755); err != nil {
		return 0, "", 0, fmt.Errorf("waiter dir: %w", err)
	}

	meta, err := os.OpenFile(m.qmetaPath(), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return 0, "", 0, fmt.Errorf("open qmeta: %w", err)
	}
	metaUnlock := func() error { return hookFlock(int(meta.Fd()), syscall.LOCK_UN) }
	metaClose := func() error { return meta.Close() }

	if err := hookFlock(int(meta.Fd()), syscall.LOCK_EX); err != nil {
		closeErr := metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("qmeta flock: %w", err), closeErr)
	}

	// Reap only confidently-dead tokens under the meta lock.
	if err := m.reapDeadWaiters(); err != nil {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(err, uerr, cerr)
	}

	ticket, err = m.nextTicketLocked()
	if err != nil {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(err, uerr, cerr)
	}

	ident, err := selfTokenIdentity()
	if err != nil {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("self identity: %w", err), uerr, cerr)
	}
	body, err := encodeTokenBody(ident)
	if err != nil {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(err, uerr, cerr)
	}

	// Publish with true O_EXCL on the final ticket name so exactly one
	// publisher wins (Stat+Rename can clobber under a race; link/rename
	// overwrite is not exclusive). Write fully-fsynced staging first so
	// readers never observe a torn JSON body under the final name, then
	// exclusive-create the final name via link (EEXIST = lost race).
	name := fmt.Sprintf("%020d", ticket)
	tokenPath = filepath.Join(qdir, name)
	stagePath := tokenPath + ".staging"
	// Under qmeta exclusive lock no concurrent publisher for this mailbox
	// is in-flight; leftover staging is crashed debris — clear it.
	if rmErr := removeTokenFn(stagePath); rmErr != nil && !os.IsNotExist(rmErr) {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token stage clear: %w", rmErr), uerr, cerr)
	}
	tf, err := os.OpenFile(stagePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token stage O_EXCL: %w", err), uerr, cerr)
	}
	if _, err := tf.Write(append(body, '\n')); err != nil {
		closeErr := tf.Close()
		rmErr := removeTokenFn(stagePath)
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token write: %w", err), closeErr, rmErr, uerr, cerr)
	}
	if err := fileSyncFn(tf); err != nil {
		closeErr := tf.Close()
		rmErr := removeTokenFn(stagePath)
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token fsync: %w", err), closeErr, rmErr, uerr, cerr)
	}
	if err := tf.Close(); err != nil {
		rmErr := removeTokenFn(stagePath)
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token close: %w", err), rmErr, uerr, cerr)
	}
	// O_EXCL of final name: link fails with EEXIST if another name already
	// holds this ticket (never overwrite via rename).
	if err := os.Link(stagePath, tokenPath); err != nil {
		rmStage := removeTokenFn(stagePath)
		uerr, cerr := metaUnlock(), metaClose()
		if os.IsExist(err) || errors.Is(err, syscall.EEXIST) {
			return 0, "", 0, errors.Join(fmt.Errorf("token O_EXCL: destination exists"), rmStage, uerr, cerr)
		}
		return 0, "", 0, errors.Join(fmt.Errorf("token publish O_EXCL link: %w", err), rmStage, uerr, cerr)
	}
	// Staging hard-link peer removed; final name alone remains.
	if rmStage := removeTokenFn(stagePath); rmStage != nil && !os.IsNotExist(rmStage) {
		// Final is published; try to roll it back so we don't leave a live
		// head we failed to finish cleanly, and surface every error.
		rmTok := removeTokenFn(tokenPath)
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token stage unlink: %w", rmStage), rmTok, uerr, cerr)
	}
	if err := syncDirFn(tokenPath); err != nil {
		// Published name exists; try to remove to avoid a stuck live head,
		// but surface both errors fail-closed.
		rmErr := removeTokenFn(tokenPath)
		uerr, cerr := metaUnlock(), metaClose()
		return 0, "", 0, errors.Join(fmt.Errorf("token dirsync: %w", err), rmErr, uerr, cerr)
	}

	// Count live under meta lock without a second reap pass that races us.
	depth, err = m.countTicketTokens()
	uerr := metaUnlock()
	cerr := metaClose()
	if err != nil {
		rmErr := removeTokenFn(tokenPath)
		return 0, "", 0, errors.Join(err, rmErr, uerr, cerr)
	}
	if uerr != nil || cerr != nil {
		rmErr := removeTokenFn(tokenPath)
		return 0, "", 0, errors.Join(uerr, cerr, rmErr)
	}
	return ticket, tokenPath, depth, nil
}

// nextTicketLocked reads/advances the crash-safe ticket counter.
// Caller holds the qmeta flock. Empty/torn/corrupt counters never reset
// below max(live tickets)+1.
func (m *Mailbox) nextTicketLocked() (int64, error) {
	path := m.ticketPath()
	maxLive, err := m.maxLiveTicket()
	if err != nil {
		return 0, err
	}

	var next int64 = 1
	data, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		s := strings.TrimSpace(string(data))
		if s == "" {
			// Empty after crash between truncate and write of old design,
			// or zero-length file: never reuse below live tickets.
			if maxLive > 0 {
				next = maxLive + 1
			} else {
				next = 1
			}
		} else {
			n, perr := strconv.ParseInt(s, 10, 64)
			if perr != nil || n < 1 {
				// Corrupt/torn decimal: recover from live tickets only.
				if maxLive > 0 {
					next = maxLive + 1
				} else {
					return 0, fmt.Errorf("corrupt ticket counter with no live tickets")
				}
			} else {
				next = n
				if maxLive >= next {
					// Counter lagged behind durable tokens — jump ahead.
					next = maxLive + 1
				}
			}
		}
	case os.IsNotExist(rerr):
		if maxLive > 0 {
			next = maxLive + 1
		} else {
			next = 1
		}
	default:
		return 0, fmt.Errorf("read ticket counter: %w", rerr)
	}

	ticket := next
	// Persist next-to-issue via atomic replace + dirsync (no truncate-in-place).
	if err := writeFileAtomic(path, []byte(strconv.FormatInt(ticket+1, 10)+"\n"), 0644); err != nil {
		return 0, fmt.Errorf("persist ticket counter: %w", err)
	}
	return ticket, nil
}

func (m *Mailbox) maxLiveTicket() (int64, error) {
	entries, err := os.ReadDir(m.waiterDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var max int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Ignore staging files and non-ticket debris.
		if !isTicketTokenName(e.Name()) {
			continue
		}
		n, perr := strconv.ParseInt(e.Name(), 10, 64)
		if perr != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max, nil
}

func isTicketTokenName(name string) bool {
	if name == "" || strings.Contains(name, ".") {
		return false // e.g. "0001.staging"
	}
	_, err := strconv.ParseInt(name, 10, 64)
	return err == nil
}

func (m *Mailbox) countTicketTokens() (int, error) {
	entries, err := os.ReadDir(m.waiterDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && isTicketTokenName(e.Name()) {
			n++
		}
	}
	return n, nil
}

// dequeueWaiter removes the token and dirsyncs the waiter directory so the
// handoff is crash-durable. Failures must surface to the caller (redacted).
func (m *Mailbox) dequeueWaiter(tokenPath string) error {
	if tokenPath == "" {
		return nil
	}
	if err := removeTokenFn(tokenPath); err != nil && !os.IsNotExist(err) {
		return redactErr(fmt.Errorf("token remove: %w", err))
	}
	// Dirsync the waiter dir so successors observe the removal after crash.
	if err := tokenDirSyncFn(m.waiterDir()); err != nil {
		// If the dir itself is gone, treat as success (nothing left to hand off).
		if os.IsNotExist(err) {
			return nil
		}
		return redactErr(fmt.Errorf("waiter dirsync after remove: %w", err))
	}
	return nil
}

// removeTokenDurable removes path and dirsyncs the waiter dir. Errors surface.
func (m *Mailbox) removeTokenDurable(path string) error {
	if err := removeTokenFn(path); err != nil && !os.IsNotExist(err) {
		return redactErr(fmt.Errorf("token remove: %w", err))
	}
	if err := tokenDirSyncFn(m.waiterDir()); err != nil && !os.IsNotExist(err) {
		return redactErr(fmt.Errorf("token remove dirsync: %w", err))
	}
	return nil
}

// reapDeadWaiters reaps confidently-dead tokens and abandoned staging
// debris. Caller MUST hold the qmeta exclusive flock so concurrent
// publishers for this mailbox cannot be mid-staging (under qmeta, any
// leftover *.staging is crashed debris and is safe to remove).
func (m *Mailbox) reapDeadWaiters() error {
	dir := m.waiterDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(dir, name)
		// Staging debris: only reap when confidently abandoned by age.
		// Never remove zero-age stages — a concurrent publisher (or a
		// test calling reap without qmeta) may be mid-write. Publishers
		// under qmeta clear their own stage path before O_EXCL create.
		if strings.HasSuffix(name, ".staging") {
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if hookNow().Sub(info.ModTime()) < stuckGrace {
				continue // in-flight or fresh crash residue — leave it
			}
			if rerr := m.removeTokenDurable(path); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		if !isTicketTokenName(name) {
			// Non-ticket debris: attempt durable remove; surface first error.
			if rerr := m.removeTokenDurable(path); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			// Unreadable: treat as ambiguous — never reap a potentially live waiter.
			continue
		}
		body, complete, perr := parseTokenBody(data)
		if perr != nil || !complete {
			// Incomplete/unreadable body: ambiguous. Never remove —
			// stuckGrace bounds permanent stalls.
			continue
		}
		switch classifyToken(body, complete) {
		case tokenDead:
			if rerr := m.removeTokenDurable(path); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
		case tokenLive, tokenAmbiguous:
			// Never silently reap live or ambiguous.
		}
	}
	return firstErr
}

// reapDeadWaitersUnderMeta tries to acquire qmeta (non-blocking), reaps dead
// tokens/staging, and releases. Used by waiters so a dead head can be
// cleared without a new enqueue. If qmeta is busy (another waiter/enqueuer
// holds it), returns nil — the next poll retries. Hard flock errors and
// unlock/close failures are joined fail-closed.
func (m *Mailbox) reapDeadWaitersUnderMeta() error {
	meta, err := os.OpenFile(m.qmetaPath(), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open qmeta for reap: %w", err)
	}
	if err := hookFlock(int(meta.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := meta.Close()
		if isLockBusy(err) {
			// Another waiter or enqueuer owns qmeta; do not block the
			// hot wait path behind 100× serial reaps.
			return closeErr
		}
		return errors.Join(fmt.Errorf("qmeta flock for reap: %w", err), closeErr)
	}
	reapErr := m.reapDeadWaiters()
	unlockErr := hookFlock(int(meta.Fd()), syscall.LOCK_UN)
	closeErr := meta.Close()
	return errors.Join(reapErr, unlockErr, closeErr)
}

// headTokenDead reports whether the current minimum live ticket token is
// confidently dead. Lock-free observation only; never reaps.
func (m *Mailbox) headTokenDead() bool {
	min, err := m.minLiveTicket()
	if err != nil || min <= 0 {
		return false
	}
	path := filepath.Join(m.waiterDir(), fmt.Sprintf("%020d", min))
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return false
	}
	body, complete, perr := parseTokenBody(data)
	if perr != nil || !complete {
		return false
	}
	return classifyToken(body, complete) == tokenDead
}

func (m *Mailbox) liveWaiterDepth() (int, error) {
	// Reap only under qmeta. Outside meta, counting must not reap —
	// concurrent publishers may be staging tokens.
	return m.countTicketTokens()
}

func (m *Mailbox) queuePosition(ticket int64) (head bool, position int, depth int, err error) {
	// Do NOT reap here: enqueue holds qmeta for reaping; concurrent
	// queuePosition from waitForTurn must only observe published tickets.
	// Head-of-line is the minimum live ticket number (FIFO), never raw
	// directory iteration order.
	entries, err := os.ReadDir(m.waiterDir())
	if err != nil {
		if os.IsNotExist(err) {
			return true, 1, 1, nil
		}
		return false, 0, 0, err
	}
	var live []int64
	for _, e := range entries {
		if e.IsDir() || !isTicketTokenName(e.Name()) {
			continue
		}
		n, perr := strconv.ParseInt(e.Name(), 10, 64)
		if perr != nil {
			continue
		}
		live = append(live, n)
	}
	depth = len(live)
	if depth == 0 {
		return true, 1, 1, nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i] < live[j] })
	position = 0
	for i, n := range live {
		if n == ticket {
			position = i + 1
			break
		}
	}
	if position == 0 {
		return false, depth + 1, depth, nil
	}
	return position == 1, position, depth, nil
}

func (m *Mailbox) waitForTurn(ctx context.Context, ticket int64, tokenPath string, start time.Time, bound time.Duration, reason, mailboxID string) error {
	lockPath := m.MailFile + ".lock"
	lastProgress := start
	lastPos := int(^uint(0) >> 1)
	lastMin := int64(0)
	lastDepth := int(^uint(0) >> 1)

	for {
		// Confirm our token still exists; a vanished token must not sit until
		// stuckGrace (that is a false "no progress" timeout, not a stall).
		if _, err := os.Stat(tokenPath); err != nil {
			return m.newLockTimeout(lockPath, mailboxID, hookNow().Sub(start), bound, lastDepth, lastPos, "lost_token")
		}

		head, pos, depth, err := m.queuePosition(ticket)
		if err != nil {
			return fmt.Errorf("mailbox lock queue inspect failed for %s: %w", mailboxID, err)
		}
		if head {
			// Head of line: still honor deadline before returning to mutate.
			if err := m.checkWaitLimits(ctx, start, lastProgress, bound, reason, lockPath, mailboxID, depth, pos); err != nil {
				return err
			}
			return nil
		}
		// queuePosition returns position=depth+1 when our ticket is absent.
		if pos > depth {
			return m.newLockTimeout(lockPath, mailboxID, hookNow().Sub(start), bound, depth, pos, "lost_token")
		}

		// Only when not head: if the HOL token looks dead, try a non-blocking
		// qmeta reap so waiters clear a dead head without a new enqueue.
		// Never thrash blocking qmeta on every poll under 100-writer load.
		if m.headTokenDead() {
			if rerr := m.reapDeadWaitersUnderMeta(); rerr != nil {
				return fmt.Errorf("mailbox lock dead-head reap failed for %s: %w", mailboxID, rerr)
			}
			// Re-evaluate position immediately after a successful reap attempt.
			continue
		}

		minTicket, _ := m.minLiveTicket()
		// Progress: our position improves, head ticket advances, or depth drops.
		if pos < lastPos || (minTicket > 0 && minTicket > lastMin) || depth < lastDepth {
			lastPos, lastMin, lastDepth = pos, minTicket, depth
			lastProgress = hookNow()
		}
		if err := m.checkWaitLimits(ctx, start, lastProgress, bound, reason, lockPath, mailboxID, depth, pos); err != nil {
			return err
		}
		hookSleep(lockPollInterval)
	}
}

func (m *Mailbox) minLiveTicket() (int64, error) {
	entries, err := os.ReadDir(m.waiterDir())
	if err != nil {
		return 0, err
	}
	var min int64
	for _, e := range entries {
		if e.IsDir() || !isTicketTokenName(e.Name()) {
			continue
		}
		n, perr := strconv.ParseInt(e.Name(), 10, 64)
		if perr != nil {
			continue
		}
		if min == 0 || n < min {
			min = n
		}
	}
	return min, nil
}

func (m *Mailbox) checkWaitLimits(ctx context.Context, start, lastProgress time.Time, bound time.Duration, reason, lockPath, mailboxID string, depth, pos int) error {
	now := hookNow()
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return m.newLockTimeout(lockPath, mailboxID, now.Sub(start), bound, depth, pos, reason+"_context")
		}
	}
	if now.Sub(lastProgress) >= bound {
		return m.newLockTimeout(lockPath, mailboxID, now.Sub(start), bound, depth, pos, reason)
	}
	if now.Sub(start) >= maxLockWait {
		return m.newLockTimeout(lockPath, mailboxID, now.Sub(start), maxLockWait, depth, pos, "max_wait")
	}
	return nil
}

func (m *Mailbox) newLockTimeout(lockPath, mailboxID string, waited, bound time.Duration, depth, pos int, reason string) error {
	owner := readLockOwnerPID(lockPath)
	if mailboxID == "" {
		mailboxID = mailboxIdentity(m.MailFile)
	}
	if strings.Contains(mailboxID, "/") || strings.Contains(mailboxID, "\\") {
		mailboxID = filepath.Base(mailboxID)
	}
	lte := &LockTimeoutError{
		MailboxID:  mailboxID,
		Waited:     waited,
		Bound:      bound,
		QueueDepth: depth,
		Position:   pos,
		OwnerPID:   owner,
		SelfPID:    os.Getpid(),
		Reason:     reason,
	}
	if derr := m.writeLockDiag(lte); derr != nil {
		return errors.Join(lte, fmt.Errorf("lock diagnostic durable write failed: %w", redactErr(derr)))
	}
	return lte
}

func (m *Mailbox) writeLockDiag(lte *LockTimeoutError) error {
	if m == nil || lte == nil {
		return errors.New("nil lock diagnostic")
	}
	diag := LockDiag{
		Status:     "BLOCKED",
		Timestamp:  hookNow().UTC(),
		MailboxID:  lte.MailboxID,
		WaitedMs:   lte.Waited.Milliseconds(),
		BoundMs:    lte.Bound.Milliseconds(),
		QueueDepth: lte.QueueDepth,
		Position:   lte.Position,
		OwnerPID:   lte.OwnerPID,
		SelfPID:    lte.SelfPID,
		Reason:     lte.Reason,
	}
	data, err := json.Marshal(diag)
	if err != nil {
		return err
	}
	diagPath := m.MailFile + ".lock.diag.jsonl"
	diagLockPath := m.MailFile + ".lock.diag.lock"

	df, err := os.OpenFile(diagLockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return redactErr(err)
	}

	deadline := hookNow().Add(diagLockBound)
	for {
		err := hookFlock(int(df.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			closeErr := df.Close()
			return redactErr(errors.Join(err, closeErr))
		}
		if hookNow().After(deadline) {
			closeErr := df.Close()
			return errors.Join(fmt.Errorf("diag lock busy"), closeErr)
		}
		hookSleep(lockPollInterval)
	}

	appendErr := appendLine(diagPath, data)
	unlockErr := hookFlock(int(df.Fd()), syscall.LOCK_UN)
	closeErr := df.Close()
	return errors.Join(appendErr, unlockErr, closeErr)
}

func writeLockOwnerPID(f *os.File) error {
	if f == nil {
		return nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	// Owner record: pid + start_ns (same liveness fingerprint as tokens).
	ident, err := selfTokenIdentity()
	if err != nil {
		// Fall back to pid-only owner record if identity unavailable;
		// diagnostics still work; reaping uses token identity primarily.
		if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
			return err
		}
		return fileSyncFn(f)
	}
	body, err := encodeTokenBody(ident)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		return err
	}
	return fileSyncFn(f)
}

func clearLockOwnerPID(f *os.File) error {
	if f == nil {
		return nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	return fileSyncFn(f)
}

func readLockOwnerPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	body, _, err := parseTokenBody(data)
	if err == nil && body.PID > 0 {
		return body.PID
	}
	// Legacy bare pid.
	s := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
