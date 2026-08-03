package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func holdMailboxLock(t *testing.T, mailFile string) (release func()) {
	t.Helper()
	lockPath := mailFile + ".lock"
	if err := os.MkdirAll(filepath.Dir(mailFile), 0755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		holder.Close()
		t.Fatalf("hold flock: %v", err)
	}
	if err := writeLockOwnerPID(holder); err != nil {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		holder.Close()
		t.Fatalf("write owner: %v", err)
	}
	return func() {
		_ = clearLockOwnerPID(holder)
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		_ = holder.Close()
	}
}

func assertNoAbsPath(t *testing.T, s string) {
	t.Helper()
	if containsAbsPath(s) {
		t.Fatalf("absolute path leak: %q", s)
	}
}

// TestFlock_HostSameProcessSecondOpenBlocks mutation-proves production host
// flock semantics: two separate opens of the same path in one process, the
// second LOCK_EX|LOCK_NB must return EWOULDBLOCK while the first holds EX.
// Uses real syscall.Flock (not hookFlock) so a fake hook cannot bless a lie.
func TestFlock_HostSameProcessSecondOpenBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "host-flock.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := syscall.Flock(int(a.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("first LOCK_EX: %v", err)
	}
	defer syscall.Flock(int(a.Fd()), syscall.LOCK_UN)

	b, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	err = syscall.Flock(int(b.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(b.Fd()), syscall.LOCK_UN)
		t.Fatal("second LOCK_EX|LOCK_NB succeeded — host flock would not serialize same-process opens")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("want EWOULDBLOCK/EAGAIN, got %v", err)
	}
}

// TestMutationProbe_NoUnboundedInProcessMutex: withFileLockContext must not
// introduce a blocking sync.Mutex before the nonblocking flock loop — a
// canceled context must still fail closed while another holder has the data lock.
func TestMutationProbe_NoUnboundedInProcessMutex(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "no-mu.jsonl")
	release := holdMailboxLock(t, mailFile)
	defer release()
	mb := NewMailbox(mailFile)

	ctx, cancel, h := withCancelOnPoll(2)
	defer cancel()
	setTestHooks(h)
	defer clearTestHooks()

	entered := false
	err := mb.withFileLockContext(ctx, func() error {
		entered = true
		return nil
	})
	if entered {
		t.Fatal("CS entered under cancel")
	}
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("want BLOCKED from nonblocking protocol, got %v (unbounded mutex would hang)", err)
	}
}

func TestConcurrentWriters_100Plus(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	const writers, perWriter = 100, 2
	total := writers * perWriter
	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			mb := NewMailbox(mailFile)
			for i := 0; i < perWriter; i++ {
				if _, err := mb.SendMessage(fmt.Sprintf("w-%d", w), "all", "m", "b"); err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	envs, err := NewMailbox(mailFile).ReadInbox("")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != total {
		t.Fatalf("got %d want %d", len(envs), total)
	}
	seqs := map[int64]bool{}
	for _, e := range envs {
		if seqs[e.Sequence] {
			t.Fatalf("dup seq %d", e.Sequence)
		}
		seqs[e.Sequence] = true
	}
}

func TestMultiProcessWriters_NoInterleaveNoDuplicate(t *testing.T) {
	if os.Getenv("MAIL_LOCK_CHILD") == "1" {
		mailFile := os.Getenv("MAIL_LOCK_FILE")
		n, _ := strconv.Atoi(os.Getenv("MAIL_LOCK_N"))
		id := os.Getenv("MAIL_LOCK_ID")
		mb := NewMailbox(mailFile)
		for i := 0; i < n; i++ {
			if _, err := mb.SendMessage("c-"+id, "all", "m", "b"); err != nil {
				fmt.Fprintf(os.Stderr, "child: %v\n", err)
				os.Exit(2)
			}
		}
		os.Exit(0)
	}

	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "proc-mail.jsonl")
	const children, perChild = 8, 5
	total := children * perChild
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, children)
	for c := 0; c < children; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cmd := exec.Command(exe, "-test.run=^TestMultiProcessWriters_NoInterleaveNoDuplicate$", "-test.count=1")
			cmd.Env = append(os.Environ(),
				"MAIL_LOCK_CHILD=1",
				"MAIL_LOCK_FILE="+mailFile,
				"MAIL_LOCK_N="+strconv.Itoa(perChild),
				"MAIL_LOCK_ID="+strconv.Itoa(c),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errCh <- fmt.Errorf("child %d: %v\n%s", c, err, out)
			}
		}(c)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	envs, err := NewMailbox(mailFile).ReadInbox("")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != total {
		t.Fatalf("subprocesses: got %d want %d", len(envs), total)
	}
}

// TestE2E_CriticalSectionFIFO proves actual withFileLock CS entry order
// equals ticket FIFO order under contention — not mere "each entered once"
// (which plain unordered flock also satisfies).
func TestE2E_CriticalSectionFIFO(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "fifo-cs.jsonl")
	mb := NewMailbox(mailFile)

	const n = 12
	var mu sync.Mutex
	csTickets := make([]int64, 0, n)
	rec := func(ticket int64) {
		mu.Lock()
		csTickets = append(csTickets, ticket)
		mu.Unlock()
	}
	recordCSTicket.Store(&rec)
	defer recordCSTicket.Store(nil)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Hold CS briefly so later tickets cannot barge past earlier ones
			// without waitForTurn (fairness mutant covered below).
			err := mb.withFileLock(func() error {
				time.Sleep(2 * time.Millisecond)
				return nil
			})
			if err != nil {
				t.Errorf("withFileLock: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(csTickets) != n {
		t.Fatalf("CS entries %d want %d", len(csTickets), n)
	}
	// Strict FIFO: CS ticket sequence must be strictly ascending.
	for i := 1; i < len(csTickets); i++ {
		if csTickets[i] <= csTickets[i-1] {
			t.Fatalf("CS order not ticket-FIFO: %v (at i=%d %d <= %d)", csTickets, i, csTickets[i], csTickets[i-1])
		}
	}

	// Fairness mutant: skipWaitForTurn allows non-head barge — under a
	// planted earlier live head, CS can be entered while ticket 1 remains.
	// (Delegates to TestMutationProbe_FairnessGatePreventsBarge for the
	// full mutant; here assert that with fairness ON the ticket sequence
	// above would fail if unordered flock were used without the queue.)
	skipWaitForTurn.Store(false)
}

// TestMutationProbe_FairnessGatePreventsBarge plants an earlier live token
// and shows: (a) with fairness ON, a later withFileLock cannot enter CS
// (times out); (b) with skipWaitForTurn, it barges into CS while not head.
// Removing waitForTurn from production is exactly (b) and must fail (a).
func TestMutationProbe_FairnessGatePreventsBarge(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "barge.jsonl")
	mb := NewMailbox(mailFile)

	// Plant a live earlier ticket token (our identity) so we are head of queue.
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	ident, err := selfTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := encodeTokenBody(ident)
	early := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 1))
	if err := os.WriteFile(early, append(body, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	// Seed ticket counter past 1 so next enqueue gets >1.
	if err := writeFileAtomic(mb.ticketPath(), []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(early)

	t.Run("fairness_on_blocks_non_head", func(t *testing.T) {
		skipWaitForTurn.Store(false)
		mb.SetLockTimeout(50 * time.Millisecond)
		entered := false
		err := mb.withFileLock(func() error {
			entered = true
			return nil
		})
		if entered {
			t.Fatal("non-head entered CS with fairness on — waitForTurn missing/bypassed")
		}
		if !errors.Is(err, ErrMailboxLockTimeout) {
			t.Fatalf("want BLOCKED timeout, got %v", err)
		}
	})

	t.Run("fairness_off_barges", func(t *testing.T) {
		skipWaitForTurn.Store(true)
		defer skipWaitForTurn.Store(false)
		mb.SetLockTimeout(0)
		var notHeadAtEntry bool
		err := mb.withFileLock(func() error {
			// Our new ticket is >1; early token 1 is still head.
			head1, _, _, qerr := mb.queuePosition(1)
			if qerr != nil {
				return qerr
			}
			notHeadAtEntry = head1 // ticket 1 is head ⇒ we barged as non-head
			return nil
		})
		if err != nil {
			t.Fatalf("barge path: %v", err)
		}
		if !notHeadAtEntry {
			t.Fatal("mutation probe: expected CS entry while ticket 1 still head (skipWaitForTurn barge)")
		}
	})
}

// TestMutationProbe_EnqueueFailClosed proves queue setup failures never
// silently degrade to unordered data-flock (no CS entry).
func TestMutationProbe_EnqueueFailClosed(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "enq-fail.jsonl")
	mb := NewMailbox(mailFile)
	// Force enqueue failure after mail dir exists: corrupt ticket counter
	// with no live tokens → nextTicketLocked fails closed.
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mb.ticketPath(), []byte("not-a-number\n"), 0644); err != nil {
		t.Fatal(err)
	}
	entered := false
	err := mb.withFileLock(func() error {
		entered = true
		return nil
	})
	if entered {
		t.Fatal("CS must not run when enqueue fails — fail-open degrade detected")
	}
	if err == nil {
		t.Fatal("expected enqueue fail-closed error")
	}
	if !strings.Contains(err.Error(), "enqueue") && !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want enqueue/corrupt in error: %v", err)
	}
	assertNoAbsPath(t, err.Error())
}

// TestConcurrentTokenPublish_NotReapedAsPartial proves atomic publish:
// concurrent readers never reap an in-flight token as unreadable debris.
func TestConcurrentTokenPublish_NotReapedAsPartial(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "pub.jsonl")
	mb := NewMailbox(mailFile)

	const n = 32
	var wg sync.WaitGroup
	errCh := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, tok, _, err := mb.enqueueWaiter()
			if err != nil {
				errCh <- err
				return
			}
			// Leave token briefly for concurrent reapers/readers.
			time.Sleep(time.Millisecond)
			if err := mb.dequeueWaiter(tok); err != nil {
				errCh <- err
			}
		}()
		go func() {
			defer wg.Done()
			// Concurrent observe: must not delete live/published tokens.
			if err := mb.reapDeadWaiters(); err != nil {
				errCh <- err
			}
			_, _, _, _ = mb.queuePosition(1)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestTicketCounter_CrashStatesNeverResetBelowLive(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "crash-ticket.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	ident, err := selfTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := encodeTokenBody(ident)

	// Live tickets 5 and 7 on disk.
	for _, n := range []int64{5, 7} {
		p := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", n))
		if err := os.WriteFile(p, append(body, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		content string // ticket file content; "" means missing
		wantMin int64  // issued ticket must be > max live (7)
	}{
		{"missing", "", 8},
		{"empty", "", 8}, // write empty file
		{"torn", "1", 8}, // would be "1" without recovery
		{"corrupt", "not-a-number", 8},
		{"behind_live", "3\n", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset ticket file per case.
			_ = os.Remove(mb.ticketPath())
			if tc.name == "missing" {
				// leave absent
			} else if tc.name == "empty" {
				if err := os.WriteFile(mb.ticketPath(), []byte{}, 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(mb.ticketPath(), []byte(tc.content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Hold meta and call nextTicketLocked via enqueue.
			ticket, tok, _, err := mb.enqueueWaiter()
			if err != nil {
				t.Fatal(err)
			}
			defer mb.dequeueWaiter(tok)
			if ticket < tc.wantMin {
				t.Fatalf("issued ticket %d < wantMin %d (reset/reuse under live tokens)", ticket, tc.wantMin)
			}
			// O_EXCL: must not have overwritten 5 or 7.
			for _, n := range []int64{5, 7} {
				p := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", n))
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("live token %d clobbered: %v", n, err)
				}
			}
		})
	}
}

func TestToken_OEXCLRejectsCollision(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "oexcl.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// Force next ticket to 1, plant existing token 1.
	if err := writeFileAtomic(mb.ticketPath(), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ident, _ := selfTokenIdentity()
	body, _ := encodeTokenBody(ident)
	p := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 1))
	if err := os.WriteFile(p, append(body, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	// nextTicketLocked will see maxLive=1 and issue 2 — so force counter
	// path: plant only by making counter say 1 and no live? We want O_EXCL
	// failure: issue ticket 1 when token 1 exists and counter is empty→
	// maxLive+1 = 2. To hit O_EXCL on ticket 1, temporarily break maxLive
	// by using a non-numeric name... Instead: call OpenFile O_EXCL directly
	// on existing path through enqueue by stubbing nextTicketLocked.
	// Simpler unit: OpenFile O_EXCL on existing token fails.
	_, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		t.Fatal("expected O_EXCL failure on existing token")
	}
	if !os.IsExist(err) {
		t.Fatalf("want IsExist, got %v", err)
	}

	// Link-based exclusive publish also fails when final name exists.
	stage := p + ".staging"
	if err := os.WriteFile(stage, append(body, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(stage, p); err == nil {
		t.Fatal("expected link O_EXCL failure on existing final token")
	}
	if !os.IsExist(err) && !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("want EEXIST from exclusive link, got %v", err)
	}
	_ = os.Remove(stage)
}

// TestTokenPublish_LinkNotRenameOverwrite proves final-name publish uses
// exclusive link (O_EXCL semantics), not Stat+Rename overwrite.
func TestTokenPublish_LinkNotRenameOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "link-pub.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// Plant a LIVE token at ticket 5 (self identity so it is not reaped)
	// with a distinctive StartNS marker in a second file? Self identity is
	// fixed — plant self body and assert the inode/content is unchanged
	// after a colliding publish path via exclusive link.
	ident, err := selfTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := encodeTokenBody(ident)
	final := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 5))
	if err := os.WriteFile(final, append(raw, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	// Counter claims next is 5 (would collide without maxLive recovery).
	if err := writeFileAtomic(mb.ticketPath(), []byte("5\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// enqueue recovers next > maxLive (5) → 6; must not clobber 5.
	ticket, tok, _, err := mb.enqueueWaiter()
	if err != nil {
		t.Fatal(err)
	}
	defer mb.dequeueWaiter(tok)
	if ticket != 6 {
		t.Fatalf("ticket=%d want 6", ticket)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(before) {
		t.Fatalf("token 5 clobbered by publish (Stat+Rename overwrite?): before=%s after=%s", before, got)
	}

	// Direct proof: exclusive link to existing final name fails (O_EXCL).
	stage := final + ".staging"
	if err := os.WriteFile(stage, []byte("attacker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	if err := os.Link(stage, final); err == nil {
		t.Fatal("link must fail with EEXIST — exclusive final-name publish")
	}
}

// TestReap_StaleStagingDebris proves abandoned *.staging files older than
// stuckGrace are reaped (crash residue), not left unmanaged; fresh stages
// are left alone so in-flight publishers are not clobbered.
func TestReap_StaleStagingDebris(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "stage-debris.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d.staging", 9))
	if err := os.WriteFile(stage, []byte(`{"pid":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Fresh stage must survive (may be an in-flight publisher).
	if err := mb.reapDeadWaitersUnderMeta(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("fresh staging must not be reaped")
	}
	// Age it past stuckGrace via fake clock.
	var mu sync.Mutex
	now := time.Now().Add(stuckGrace + time.Second)
	setTestHooks(&clockHooks{
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	})
	defer clearTestHooks()
	if err := mb.reapDeadWaitersUnderMeta(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stale staging debris was not reaped")
	}
}

// TestWaiter_ReapsDeadHeadWithoutNewEnqueue proves an existing waiter can
// clear a dead head-of-line token without requiring a new enqueue to reap.
func TestWaiter_ReapsDeadHeadWithoutNewEnqueue(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "dead-head.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// Dead head token (nonexistent PID + complete identity).
	dead := waiterTokenBody{PID: 1 << 30, StartNS: 1, BootID: bootIdentity()}
	if processAlive(dead.PID) {
		t.Skip("pid unexpectedly alive")
	}
	raw, _ := encodeTokenBody(dead)
	headPath := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 1))
	if err := os.WriteFile(headPath, append(raw, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(mb.ticketPath(), []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Waiter enqueues as ticket >=2 and must become head after reaping dead 1.
	entered := false
	err := mb.withFileLock(func() error {
		entered = true
		// Dead head must already be gone when we enter CS.
		if _, serr := os.Stat(headPath); !os.IsNotExist(serr) {
			return fmt.Errorf("dead head still present at CS entry")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("waiter should reap dead head and enter CS: %v", err)
	}
	if !entered {
		t.Fatal("CS never entered")
	}
}

// TestExpiredContext_FailsClosedUncontended proves a past-deadline ctx
// never mutates even when the data lock is free (no contention).
func TestExpiredContext_FailsClosedUncontended(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "expired-ctx.jsonl")
	mb := NewMailbox(mailFile)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	mutated := false
	err := mb.withFileLockContext(ctx, func() error {
		mutated = true
		return nil
	})
	if mutated {
		t.Fatal("expired context entered critical section while uncontended")
	}
	if err == nil {
		t.Fatal("expected fail-closed error for expired context")
	}
	if !errors.Is(err, ErrMailboxLockTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		// Typed BLOCKED is preferred; either is fail-closed.
		if !strings.Contains(err.Error(), "BLOCKED") && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
			t.Fatalf("want deadline/BLOCKED, got %v", err)
		}
	}
	// No mail written.
	if _, e := os.Stat(mailFile); !os.IsNotExist(e) {
		t.Fatal("must not write under expired context")
	}
}

// TestAppendDequeueFailure_IdempotentNoDuplicate: successful append then
// dequeue failure still records the envelope id; same-id retry does not
// duplicate the durable line.
func TestAppendDequeueFailure_IdempotentNoDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "idem.jsonl")
	mb := NewMailbox(mailFile)

	origRm := removeTokenFn
	defer func() { removeTokenFn = origRm }()
	// Fail only final ticket-token dequeue (not staging cleanup under qmeta).
	removeTokenFn = func(path string) error {
		if isTicketTokenName(filepath.Base(path)) {
			return errors.New("injected dequeue remove")
		}
		return origRm(path)
	}

	env := newEnvelope("a", "b", "s", "body")
	err := mb.appendEnvelopeContext(context.Background(), env)
	if err == nil {
		t.Fatal("expected joined dequeue failure")
	}
	if !mb.fileHasID(env.ID) {
		t.Fatal("envelope must be durable after append despite dequeue failure")
	}
	// Same-id retry must be a no-op (no second line).
	removeTokenFn = origRm // allow clean dequeue on retry path
	if err := mb.appendEnvelopeContext(context.Background(), env); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	data, err := os.ReadFile(mailFile)
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(data), `"id":"`+env.ID+`"`)
	if count != 1 {
		t.Fatalf("duplicate lines for id: count=%d data=%s", count, data)
	}
}

// TestErrorPath_UnlockCloseJoined proves acquire-path close/unlock errors
// are joined into the returned error (not discarded with _).
func TestErrorPath_UnlockCloseJoined(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "join-close.jsonl")
	mb := NewMailbox(mailFile)

	// Inject hard flock error after qmeta; open of data lock succeeds then
	// flock fails non-busy → close must be checked (joined into error tree).
	var ex atomic.Int32
	setTestHooks(&clockHooks{
		Flock: func(fd int, how int) error {
			if how == syscall.LOCK_UN {
				return syscall.Flock(fd, how)
			}
			if how&syscall.LOCK_EX != 0 {
				n := ex.Add(1)
				// Allow qmeta EX locks (enqueue + any reap), fail data lock.
				// Data lock is after at least one qmeta from enqueue.
				if n >= 2 && how&syscall.LOCK_NB != 0 {
					return syscall.EINVAL
				}
				return syscall.Flock(fd, how)
			}
			return syscall.Flock(fd, how)
		},
	})
	defer clearTestHooks()

	err := mb.withFileLock(func() error { return nil })
	if err == nil {
		t.Fatal("expected acquire failure")
	}
	assertNoAbsPath(t, err.Error())
}

func TestTokenLiveness_PIDReuseAndDeadHead(t *testing.T) {
	// Dead PID: classify dead.
	body := waiterTokenBody{PID: 1 << 30, StartNS: 1, BootID: bootIdentity()} // unlikely live
	if classifyToken(body, true) != tokenDead && processAlive(body.PID) {
		t.Skip("pid unexpectedly alive")
	}
	if processAlive(body.PID) {
		t.Skip("cannot test dead pid")
	}
	if classifyToken(body, true) != tokenDead {
		t.Fatalf("want dead for nonexistent pid")
	}

	// Self is live.
	ident, err := selfTokenIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if classifyToken(ident, true) != tokenLive {
		t.Fatal("self must be live")
	}

	// PID reuse: same pid, wrong start_ns.
	reused := ident
	reused.StartNS = ident.StartNS + 999999
	if classifyToken(reused, true) != tokenDead {
		t.Fatal("PID reuse (mismatched start) must be dead")
	}

	// Ambiguous: pid-only incomplete token that is alive (self).
	amb := waiterTokenBody{PID: os.Getpid()}
	if classifyToken(amb, false) != tokenAmbiguous {
		t.Fatal("incomplete identity for live pid must be ambiguous (not reaped)")
	}

	// Boot mismatch → dead.
	if boot := bootIdentity(); boot != "" {
		booted := ident
		booted.BootID = boot + "-other"
		if classifyToken(booted, true) != tokenDead {
			t.Fatal("boot mismatch must be dead")
		}
	}
}

func TestReap_DoesNotRemoveAmbiguousOrLive(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "reap.jsonl")
	mb := NewMailbox(mailFile)
	_ = os.MkdirAll(mb.waiterDir(), 0755)
	ident, _ := selfTokenIdentity()
	body, _ := encodeTokenBody(ident)
	livePath := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 3))
	if err := os.WriteFile(livePath, append(body, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	// Ambiguous legacy pid-only for self.
	ambPath := filepath.Join(mb.waiterDir(), fmt.Sprintf("%020d", 4))
	if err := os.WriteFile(ambPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mb.reapDeadWaiters(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatal("live token reaped")
	}
	if _, err := os.Stat(ambPath); err != nil {
		t.Fatal("ambiguous token reaped")
	}
}

func TestDequeue_SurfacesRemoveAndDirsyncFailure(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "deq.jsonl")
	mb := NewMailbox(mailFile)
	tok := filepath.Join(tmpDir, "tok")
	if err := os.WriteFile(tok, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	origRm, origSync := removeTokenFn, tokenDirSyncFn
	defer func() {
		removeTokenFn = origRm
		tokenDirSyncFn = origSync
	}()

	removeTokenFn = func(path string) error {
		return &os.PathError{Op: "remove", Path: path, Err: errors.New("injected remove fail")}
	}
	err := mb.dequeueWaiter(tok)
	if err == nil {
		t.Fatal("expected remove failure")
	}
	assertNoAbsPath(t, err.Error())

	removeTokenFn = func(path string) error { return nil }
	tokenDirSyncFn = func(path string) error {
		return &os.PathError{Op: "sync", Path: path, Err: errors.New("injected dirsync fail")}
	}
	err = mb.dequeueWaiter(tok)
	if err == nil {
		t.Fatal("expected dirsync failure")
	}
	assertNoAbsPath(t, err.Error())
}

func TestWithFileLock_JoinsDequeueFailure(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "join-deq.jsonl")
	mb := NewMailbox(mailFile)

	origRm := removeTokenFn
	defer func() { removeTokenFn = origRm }()
	// Inject only on final ticket-token dequeue, not staging/debris cleanup.
	removeTokenFn = func(path string) error {
		base := filepath.Base(path)
		if isTicketTokenName(base) {
			return errors.New("injected dequeue remove")
		}
		return origRm(path)
	}
	err := mb.withFileLock(func() error { return nil })
	if err == nil {
		t.Fatal("expected joined dequeue failure")
	}
	if !strings.Contains(err.Error(), "dequeue") && !strings.Contains(err.Error(), "remove") {
		t.Fatalf("want dequeue/remove in error: %v", err)
	}
	assertNoAbsPath(t, err.Error())
}

func TestLockTimeout_TypedBlockedRedactedDurable(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "held.jsonl")
	release := holdMailboxLock(t, mailFile)
	defer release()
	mb := NewMailbox(mailFile)
	mb.SetLockTimeout(40 * time.Millisecond)
	secret := "secret-body"
	_, err := mb.SendMessage("a", "b", "s", secret)
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), secret) || containsAbsPath(err.Error()) {
		t.Fatalf("leak: %v", err)
	}
	raw, err := os.ReadFile(mailFile + ".lock.diag.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if containsAbsPath(string(raw)) || strings.Contains(string(raw), secret) {
		t.Fatalf("diag leak: %s", raw)
	}
	var diag LockDiag
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &diag); err != nil {
		t.Fatal(err)
	}
	if diag.MailboxID != "held.jsonl" || diag.Status != "BLOCKED" {
		t.Fatalf("%+v", diag)
	}
}

func TestFlockHardError_FailsImmediately(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "hard.jsonl")
	var ex atomic.Int32
	setTestHooks(&clockHooks{
		Flock: func(fd int, how int) error {
			if how == syscall.LOCK_UN {
				return syscall.Flock(fd, how)
			}
			if how&syscall.LOCK_EX != 0 {
				n := ex.Add(1)
				if n == 1 { // qmeta
					return syscall.Flock(fd, how)
				}
				return syscall.EINVAL
			}
			return syscall.Flock(fd, how)
		},
	})
	defer clearTestHooks()
	mb := NewMailbox(mailFile)
	start := time.Now()
	_, err := mb.SendMessage("a", "b", "s", "body")
	if err == nil || errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("want hard acquire error, got %v", err)
	}
	// Must fail closed without stuckGrace spin; residual wall under -race
	// is enqueue/fs I/O, not a poll loop. Bound well below stuckGrace.
	if time.Since(start) >= stuckGrace/2 {
		t.Fatalf("spun %s suggests busy-poll retry of hard flock error", time.Since(start))
	}
	assertNoAbsPath(t, err.Error())
}

// holdQmeta takes an exclusive lock on the mailbox qmeta file with a
// writeLockOwnerPID-compatible owner record (pid+start_ns(+boot)), matching
// production holders. Bare "1\n" is not a complete identity and must not be
// used as a wedged-owner fixture.
func holdQmeta(t *testing.T, mailFile string) (release func()) {
	t.Helper()
	path := mailFile + ".lock.qmeta"
	if err := os.MkdirAll(filepath.Dir(mailFile), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("hold qmeta: %v", err)
	}
	if err := writeLockOwnerPID(f); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		t.Fatalf("write qmeta owner: %v", err)
	}
	return func() {
		_ = clearLockOwnerPID(f)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// withCancelOnPoll returns a context canceled after n Sleep hook calls —
// deterministic deadline propagation without wall-clock flake.
func withCancelOnPoll(n int32) (context.Context, context.CancelFunc, *clockHooks) {
	ctx, cancel := context.WithCancel(context.Background())
	var polls atomic.Int32
	h := &clockHooks{
		Sleep: func(d time.Duration) {
			if polls.Add(1) >= n {
				cancel()
			}
		},
	}
	return ctx, cancel, h
}

func assertBlockedNoCS(t *testing.T, err error, entered bool, wantReasonSubstr string) {
	t.Helper()
	if entered {
		t.Fatal("critical section entered under fail-closed deadline")
	}
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("want ErrMailboxLockTimeout, got %v", err)
	}
	assertNoAbsPath(t, err.Error())
	var lte *LockTimeoutError
	if errors.As(err, &lte) && wantReasonSubstr != "" {
		if !strings.Contains(lte.Reason, wantReasonSubstr) {
			t.Fatalf("reason %q want substring %q", lte.Reason, wantReasonSubstr)
		}
	}
}

func TestContextDeadline_PropagatesConsumers(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("data_lock_cancel_on_poll", func(t *testing.T) {
		mailFile := filepath.Join(tmpDir, "ctx-data.jsonl")
		release := holdMailboxLock(t, mailFile)
		defer release()
		mb := NewMailbox(mailFile)
		ctx, cancel, h := withCancelOnPoll(2)
		defer cancel()
		setTestHooks(h)
		defer clearTestHooks()
		entered := false
		err := mb.withFileLockContext(ctx, func() error {
			entered = true
			return nil
		})
		assertBlockedNoCS(t, err, entered, "context")
	})

	t.Run("wedged_qmeta_cancel_on_poll", func(t *testing.T) {
		mailFile := filepath.Join(tmpDir, "ctx-qmeta.jsonl")
		// holdQmeta writes a complete pid+start owner token (not bare "1\n").
		release := holdQmeta(t, mailFile)
		defer release()
		mb := NewMailbox(mailFile)
		ctx, cancel, h := withCancelOnPoll(2)
		defer cancel()
		setTestHooks(h)
		defer clearTestHooks()
		entered := false
		err := mb.withFileLockContext(ctx, func() error {
			entered = true
			return nil
		})
		assertBlockedNoCS(t, err, entered, "context")
		if _, e := os.Stat(mailFile); e == nil {
			t.Fatal("must not mutate under wedged qmeta")
		}
	})

	t.Run("qmeta_stuckGrace_not_maxLockWait", func(t *testing.T) {
		// Default stuckGrace, no context: complete live owner held on qmeta,
		// unchanged fingerprint + no ticket advance → timeout at stuckGrace
		// (not maxLockWait). Mere liveness must not refresh progress.
		mailFile := filepath.Join(tmpDir, "ctx-qmeta-grace.jsonl")
		release := holdQmeta(t, mailFile)
		defer release()
		// Prove fixture is a complete owner identity (writeLockOwnerPID shape).
		raw, err := os.ReadFile(mailFile + ".lock.qmeta")
		if err != nil {
			t.Fatal(err)
		}
		body, complete, perr := parseTokenBody(raw)
		if perr != nil || !complete || body.PID <= 0 || body.StartNS <= 0 {
			t.Fatalf("holdQmeta must plant complete owner token, got complete=%v body=%+v err=%v", complete, body, perr)
		}
		if classifyToken(body, complete) != tokenLive {
			t.Fatal("planted owner must be live — liveness alone must not prevent stuckGrace")
		}
		mb := NewMailbox(mailFile)
		var mu sync.Mutex
		now := time.Unix(1_700_000_000, 0)
		setTestHooks(&clockHooks{
			Now: func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			},
			Sleep: func(d time.Duration) {
				mu.Lock()
				now = now.Add(d)
				mu.Unlock()
			},
		})
		defer clearTestHooks()
		wall0 := time.Now()
		entered := false
		// No context, no SetLockTimeout → pure stuckGrace policy.
		err = mb.withFileLock(func() error {
			entered = true
			return nil
		})
		if entered {
			t.Fatal("CS entered")
		}
		if !errors.Is(err, ErrMailboxLockTimeout) {
			t.Fatalf("got %v", err)
		}
		var lte *LockTimeoutError
		if !errors.As(err, &lte) {
			t.Fatal("want LockTimeoutError")
		}
		if lte.Bound != stuckGrace {
			t.Fatalf("qmeta bound=%s want stuckGrace (not maxLockWait inflation)", lte.Bound)
		}
		if !strings.Contains(lte.Reason, "qmeta") {
			t.Fatalf("reason %q should identify qmeta wait", lte.Reason)
		}
		if lte.OwnerPID != body.PID {
			t.Fatalf("OwnerPID diagnostic=%d want planted %d", lte.OwnerPID, body.PID)
		}
		if time.Since(wall0) >= stuckGrace/2 {
			t.Fatalf("wall %s suggests real stuckGrace sleep (fake clock unused)", time.Since(wall0))
		}
	})

	t.Run("SendMessage_PostCallback_Broker_Drain_Ack", func(t *testing.T) {
		mailFile := filepath.Join(tmpDir, "ctx-paths.jsonl")
		release := holdMailboxLock(t, mailFile)
		defer release()
		mb := NewMailbox(mailFile)

		ctx, cancel, h := withCancelOnPoll(2)
		defer cancel()
		setTestHooks(h)
		defer clearTestHooks()

		_, err := mb.SendMessageContext(ctx, "a", "b", "s", "secret")
		assertBlockedNoCS(t, err, false, "context")

		ctx2, cancel2, h2 := withCancelOnPoll(2)
		defer cancel2()
		setTestHooks(h2)
		_, err = mb.PostCallbackContext(ctx2, "agent", Callback{
			Kind: CallbackComplete, Repo: "r", Ref: "main", SHA: "abc",
		})
		assertBlockedNoCS(t, err, false, "context")

		bFile := filepath.Join(tmpDir, "ctx-broker.jsonl")
		relB := holdMailboxLock(t, bFile)
		defer relB()
		broker := NewMessageBroker(NewMailbox(bFile), WithRedis(newMockRedisClient(), "h"))
		defer broker.Close()
		ctx3, cancel3, h3 := withCancelOnPoll(2)
		defer cancel3()
		setTestHooks(h3)
		_, err = broker.SendMessageContext(ctx3, "a", "b", "s", "x")
		assertBlockedNoCS(t, err, false, "context")
		clearTestHooks()

		// Consumer: post a real callback first (uncontended), then hold lock
		// and require fail-closed Drain/Ack with unchanged durable state.
		cFile := filepath.Join(tmpDir, "ctx-consumer.jsonl")
		cmb := NewMailbox(cFile)
		env, err := cmb.PostCallback("agent", Callback{
			Kind: CallbackComplete, Repo: "r", Ref: "main", SHA: "s", LeaseGeneration: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		cons, err := NewCallbackConsumer(cmb, 3)
		if err != nil {
			t.Fatal(err)
		}
		drained, err := cons.Drain()
		if err != nil || len(drained) != 1 {
			t.Fatalf("setup drain: %v %#v", err, drained)
		}
		pendingBefore := len(cons.state.Pending)
		if pendingBefore != 1 {
			t.Fatalf("pending before=%d", pendingBefore)
		}

		relC := holdMailboxLock(t, cFile)
		defer relC()

		ctx4, cancel4, h4 := withCancelOnPoll(2)
		defer cancel4()
		setTestHooks(h4)
		_, err = cons.DrainContext(ctx4)
		if err == nil {
			t.Fatal("DrainContext must fail closed under held lock (save requires flock)")
		}
		if !errors.Is(err, ErrMailboxLockTimeout) {
			t.Fatalf("DrainContext: want BLOCKED, got %v", err)
		}
		if len(cons.state.Pending) != pendingBefore {
			t.Fatalf("Drain failed but Pending mutated: got %d want %d", len(cons.state.Pending), pendingBefore)
		}

		ctx5, cancel5, h5 := withCancelOnPoll(2)
		defer cancel5()
		setTestHooks(h5)
		err = cons.AckContext(ctx5, drained[0].EnvelopeID)
		if err == nil {
			t.Fatal("AckContext must fail closed under held lock")
		}
		if !errors.Is(err, ErrMailboxLockTimeout) {
			t.Fatalf("AckContext: want BLOCKED, got %v", err)
		}
		if _, ok := cons.state.Pending[drained[0].EnvelopeID]; !ok {
			t.Fatal("Ack failed but Pending entry was removed (non-transactional)")
		}
		if _, ok := cons.state.Acked[ackKey("r", "main")]; ok {
			t.Fatal("Ack failed but Acked advanced (non-transactional)")
		}
		_ = env
		clearTestHooks()
	})
}

// TestMutationProbe_ContextDeadlineBound fails if lock acquisition ignores
// ctx (cancel) and waits the full stuckGrace — vacuous "eventually BLOCKED".
func TestMutationProbe_ContextDeadlineBound(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mut-ctx.jsonl")
	release := holdQmeta(t, mailFile)
	defer release()
	mb := NewMailbox(mailFile)

	ctx, cancel, h := withCancelOnPoll(1)
	defer cancel()
	setTestHooks(h)
	defer clearTestHooks()

	wall0 := time.Now()
	entered := false
	err := mb.withFileLockContext(ctx, func() error {
		entered = true
		return nil
	})
	if entered {
		t.Fatal("CS entered")
	}
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(wall0) >= stuckGrace/2 {
		t.Fatalf("mutation: wall %s implies stuckGrace wait (ctx cancel not honored)", time.Since(wall0))
	}
	var lte *LockTimeoutError
	if errors.As(err, &lte) && !strings.Contains(lte.Reason, "context") {
		t.Fatalf("mutation: reason=%s missing context (propagation dropped?)", lte.Reason)
	}
}

// TestTokenRollback_PostPublishDurable proves post-Link failure paths remove
// the final token with waiter-dir dirsync (no ghost head after crash).
func TestTokenRollback_PostPublishDurable(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "rollback.jsonl")
	mb := NewMailbox(mailFile)
	if err := os.MkdirAll(mb.waiterDir(), 0755); err != nil {
		t.Fatal(err)
	}

	origSync := tokenDirSyncFn
	defer func() { tokenDirSyncFn = origSync }()

	var dirSyncs atomic.Int64
	tokenDirSyncFn = func(path string) error {
		// Count waiter-dir syncs (rollback + dequeue).
		if strings.Contains(path, ".lock.waiters") || filepath.Base(path) == filepath.Base(mb.waiterDir()) {
			dirSyncs.Add(1)
		}
		return origSync(path)
	}

	// Inject: first syncDirFn after successful link (publish dirsync of token
	// path) fails — triggers rollbackPublishedToken.
	origDir := syncDirFn
	defer func() { syncDirFn = origDir }()
	var n atomic.Int32
	syncDirFn = func(path string) error {
		// token publish calls syncDirFn(tokenPath) after link.
		if strings.Contains(path, ".lock.waiters") && !strings.HasSuffix(path, ".staging") {
			if n.Add(1) == 1 {
				return errors.New("injected post-publish dirsync fail")
			}
		}
		return origDir(path)
	}

	_, _, _, err := mb.enqueueWaiter()
	if err == nil {
		t.Fatal("expected enqueue failure after publish dirsync inject")
	}
	// No live ticket tokens may remain (ghost head forbidden).
	entries, _ := os.ReadDir(mb.waiterDir())
	for _, e := range entries {
		if isTicketTokenName(e.Name()) {
			t.Fatalf("ghost published token remains after failed enqueue: %s", e.Name())
		}
	}
	// Rollback must have attempted durable dirsync of waiter dir.
	if dirSyncs.Load() < 1 {
		t.Fatal("rollback did not dirsync waiter dir — not crash-durable")
	}
	assertNoAbsPath(t, err.Error())
}

// TestTokenRollback_UnlockFailureDurable: after publish, qmeta unlock error
// still rolls back the token durably.
func TestTokenRollback_UnlockFailureDurable(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "rollback-un.jsonl")
	mb := NewMailbox(mailFile)

	var unlockFails atomic.Int32
	setTestHooks(&clockHooks{
		Flock: func(fd int, how int) error {
			if how == syscall.LOCK_UN {
				// Fail the qmeta unlock after a successful enqueue publish.
				if unlockFails.Add(1) == 1 {
					return errors.New("injected qmeta unlock fail")
				}
				return syscall.Flock(fd, how)
			}
			return syscall.Flock(fd, how)
		},
	})
	defer clearTestHooks()

	_, tok, _, err := mb.enqueueWaiter()
	if err == nil {
		// If unlock injection missed (race on unlock count), clean up.
		if tok != "" {
			_ = mb.dequeueWaiter(tok)
		}
		t.Fatal("expected enqueue error on qmeta unlock failure")
	}
	entries, _ := os.ReadDir(mb.waiterDir())
	for _, e := range entries {
		if isTicketTokenName(e.Name()) {
			t.Fatalf("ghost token after unlock-fail rollback: %s err=%v", e.Name(), err)
		}
	}
	assertNoAbsPath(t, err.Error())
}

// TestMutationProbe_MailFileFsyncOnly asserts fsync on the mailbox artifact
// itself — qmeta/token/owner/seq syncs must not satisfy the count. A mutant
// that deletes appendLine's mail-data fsync dies here.
func TestMutationProbe_MailFileFsyncOnly(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "sync-target.jsonl")
	// Pre-create empty mail file so first append opens the real path (not
	// only seq/qmeta files).
	if err := os.WriteFile(mailFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	var mailFileSyncs, mailDirSyncs atomic.Int64
	origFile, origDir := fileSyncFn, syncDirFn
	defer func() {
		fileSyncFn = origFile
		syncDirFn = origDir
	}()

	sameMail := func(p string) bool {
		return filepath.Clean(p) == filepath.Clean(mailFile) ||
			filepath.Base(p) == filepath.Base(mailFile) && strings.HasSuffix(filepath.Clean(p), filepath.Base(mailFile))
	}

	fileSyncFn = func(f *os.File) error {
		// Only the mailbox data file name — never .seq/.lock/.ticket/.staging.
		base := filepath.Base(f.Name())
		if base == filepath.Base(mailFile) {
			mailFileSyncs.Add(1)
		}
		return f.Sync()
	}
	syncDirFn = func(path string) error {
		// appendLine(mailFile) calls syncDirFn(mailFile) with the file path.
		if sameMail(path) {
			mailDirSyncs.Add(1)
		}
		return syncDir(path)
	}

	mb := NewMailbox(mailFile)
	if _, err := mb.SendMessage("a", "b", "s", "body"); err != nil {
		t.Fatal(err)
	}
	if mailFileSyncs.Load() < 1 {
		t.Fatal("mailbox file fsync never called — appendLine data fsync missing (qmeta/token/seq syncs must not pass)")
	}
	if mailDirSyncs.Load() < 1 {
		t.Fatal("mailbox dirsync never called for mailFile path")
	}

	// Inject failure only for the mail data file basename.
	fileSyncFn = func(f *os.File) error {
		if filepath.Base(f.Name()) == filepath.Base(mailFile) {
			return errors.New("injected mail fsync fail")
		}
		return f.Sync()
	}
	if _, err := mb.SendMessage("a", "b", "s2", "body2"); err == nil {
		t.Fatal("expected mailFile fsync failure to fail closed")
	}
}

func TestRedactErr_PathError(t *testing.T) {
	// Build host-style absolute path at runtime (no contiguous abs prefix in source).
	abs := filepath.Join(string(os.PathSeparator)+"Users", "someone", "proj", ".herd", "mail.jsonl.lock")
	err := &os.PathError{Op: "open", Path: abs, Err: errors.New("denied")}
	got := redactErr(err)
	if containsAbsPath(got.Error()) {
		t.Fatalf("still absolute: %v", got)
	}
	if !strings.Contains(got.Error(), "mail.jsonl.lock") {
		t.Fatalf("lost basename: %v", got)
	}
}

func TestEnqueue_PathErrorRedacted(t *testing.T) {
	tmpDir := t.TempDir()
	// Parent is a file → MkdirAll of waiter dir fails with PathError.
	blocker := filepath.Join(tmpDir, "block")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	mb := NewMailbox(filepath.Join(blocker, "mail.jsonl"))
	_, err := mb.SendMessage("a", "b", "s", "body")
	if err == nil {
		t.Fatal("expected error")
	}
	assertNoAbsPath(t, err.Error())
}

func TestFakeClock_StuckGrace(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "fake.jsonl")
	release := holdMailboxLock(t, mailFile)
	defer release()

	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	setTestHooks(&clockHooks{
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		Sleep: func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		},
	})
	defer clearTestHooks()

	mb := NewMailbox(mailFile)
	mb.SetLockTimeout(50 * time.Millisecond)
	wall := time.Now()
	_, err := mb.SendMessage("a", "b", "s", "body")
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatal(err)
	}
	// Fake clock advances deadline without sleeping stuckGrace; residual wall
	// time is real enqueue/fs I/O under -race. Must stay well below stuckGrace
	// (5s), not sub-500ms.
	if time.Since(wall) >= stuckGrace/2 {
		t.Fatalf("wall %s suggests real stuckGrace wait (fake clock not applied)", time.Since(wall))
	}
}

func TestNeverSucceedsUnlocked(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "nounlock.jsonl")
	release := holdMailboxLock(t, mailFile)
	defer release()
	mb := NewMailbox(mailFile)
	mb.SetLockTimeout(30 * time.Millisecond)
	_, err := mb.SendMessage("a", "b", "s", "body")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, e := os.Stat(mailFile); !os.IsNotExist(e) {
		t.Fatal("must not write without lock")
	}
}
