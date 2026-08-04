package toolchild

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func initRepo(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", root}, {"-C", root, "config", "user.name", "test"}, {"-C", root, "config", "user.email", "test@example.invalid"}, {"-C", root, "remote", "add", "origin", "https://example.invalid/" + name + ".git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	return root
}

func TestRepositoryIdentityDoesNotUseCollidingDisplayNames(t *testing.T) {
	a := initRepo(t, "alpha")
	b := initRepo(t, "beta")
	ida, err := RepositoryIdentity(a)
	if err != nil {
		t.Fatal(err)
	}
	idb, err := RepositoryIdentity(b)
	if err != nil {
		t.Fatal(err)
	}
	if ida == idb {
		t.Fatalf("repository identities collided: %q", ida)
	}
}

func TestRepositoryIdentityNormalizesTransportAndCredentials(t *testing.T) {
	root := initRepo(t, "canonical")
	forms := []string{"https://user:secret@Example.com/Org/Repo.git", "ssh://git@EXAMPLE.COM/Org/Repo.git", "git@example.com:Org/Repo.git"}
	var want string
	for n, form := range forms {
		if out, err := exec.Command("git", "-C", root, "config", "remote.origin.url", form).CombinedOutput(); err != nil {
			t.Fatalf("set remote %d: %s: %v", n, out, err)
		}
		got, err := RepositoryIdentity(root)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			want = got
		} else if got != want {
			t.Fatalf("transport %q normalized to %q, want %q", form, got, want)
		}
	}
}

func TestNextSessionGenerationReservesUniqueValuesConcurrently(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	const n = 8
	values := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := NextSessionGeneration("repo-concurrent")
			if err != nil {
				t.Error(err)
				return
			}
			values <- v
		}()
	}
	wg.Wait()
	close(values)
	seen := map[int64]bool{}
	for v := range values {
		if seen[v] {
			t.Fatalf("duplicate reserved generation %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("reserved %d generations, want %d", len(seen), n)
	}
}

func TestSessionReservationSurvivesRestartLoad(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	generation, err := NextSessionGeneration("repo-restart")
	if err != nil {
		t.Fatal(err)
	}
	path, err := StableReceiptPath("repo-restart")
	if err != nil {
		t.Fatal(err)
	}
	ow := owner()
	ow.SessionGeneration = generation
	ow.Repository, ow.TabID, ow.PaneID, ow.SessionID, ow.TaskRef, ow.Lane = "repo-restart", "tab-restart", "pane-restart", "session", "task", "lane"
	sink := &JSONLSink{Path: path}
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLifecycle(path, ow.TabID, &FakeTree{}, sink)
	if err != nil {
		t.Fatalf("reservation poisoned restart recovery: %v", err)
	}
	if l.Inventory.Owner.SessionGeneration != generation {
		t.Fatalf("generation=%d want %d", l.Inventory.Owner.SessionGeneration, generation)
	}
}

func TestCorruptLedgerQuarantinePreservesHistoryAndBlocksReservation(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	if generation, err := NextSessionGeneration("repo-quarantine"); err != nil || generation != 1 {
		t.Fatalf("initial reservation=%d err=%v", generation, err)
	}
	path, err := StableReceiptPath("repo-quarantine")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"action":"session-reservation"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NextSessionGeneration("repo-quarantine"); err == nil {
		t.Fatal("corrupt ledger reservation was accepted")
	}
	if _, err := os.Stat(path + ".quarantine"); err != nil {
		t.Fatalf("corrupt evidence was not quarantined: %v", err)
	}
	if _, err := NextSessionGeneration("repo-quarantine"); err == nil {
		t.Fatal("later reservation bypassed corrupt ledger")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("corrupt ledger history was overwritten")
	}
}

func TestReceiptWriteAndSyncFaultsQuarantinePartialEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*os.File, []byte) (int, error)
		sync  func(*os.File) error
	}{
		{name: "partial-with-error", write: func(f *os.File, b []byte) (int, error) {
			n, _ := f.Write(b[:len(b)-1])
			return n, errors.New("injected partial write")
		}, sync: func(*os.File) error { return nil }},
		{name: "fsync-error", write: nil, sync: func(*os.File) error { return errors.New("injected durability failure") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/receipts.jsonl"
			restore := setReceiptIOForTest(tc.write, tc.sync)
			defer restore()
			if err := (&JSONLSink{Path: path}).Write(Receipt{Action: "owner", Identity: owner()}); err == nil {
				t.Fatal("fault was accepted")
			}
			if _, err := os.Stat(path + ".quarantine"); err != nil {
				t.Fatalf("fault evidence was not quarantined: %v", err)
			}
		})
	}
}

func TestReservationUsesValidQuarantineHistoryAfterOriginalRecovery(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	if generation, err := NextSessionGeneration("repo-recovered"); err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	path, err := StableReceiptPath("repo-recovered")
	if err != nil {
		t.Fatal(err)
	}
	if err := quarantineReceipt(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	next, err := NextSessionGeneration("repo-recovered")
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("recovered generation=%d want 2", next)
	}
}

func TestPartialReservationAfterGenerationNRecoversAboveHighWater(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	if generation, err := NextSessionGeneration("repo-partial-recovery"); err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	path, err := StableReceiptPath("repo-partial-recovery")
	if err != nil {
		t.Fatal(err)
	}
	restore := setReceiptIOForTest(func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)-1])
		return n, errors.New("injected reservation partial")
	}, nil)
	_, reservationErr := NextSessionGeneration("repo-partial-recovery")
	restore()
	if reservationErr == nil {
		t.Fatal("partial reservation was accepted")
	}
	if _, err := os.Stat(path + ".quarantine"); err != nil {
		t.Fatalf("partial reservation evidence missing: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} // explicit authenticated repair step
	next, err := NextSessionGeneration("repo-partial-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if next <= 1 {
		t.Fatalf("recovered reservation reused generation: %d", next)
	}
	if _, err := os.Stat(path + ".quarantine"); err != nil {
		t.Fatalf("corrupt evidence was lost: %v", err)
	}
}

func TestDirectoryDurabilityFailureBlocksBeforeReservationReturn(t *testing.T) {
	t.Setenv("HERD_TOOLCHILD_RECEIPT_ROOT", t.TempDir())
	path, err := StableReceiptPath("repo-dir-failure")
	if err != nil {
		t.Fatal(err)
	}
	restore := setDirectorySyncForTest(func(string) error { return errors.New("injected directory fsync failure") })
	if _, err := NextSessionGeneration("repo-dir-failure"); err == nil {
		t.Fatal("generation returned without directory durability")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("reservation was appended before high-water directory durability: %v", statErr)
	}
	restore()
	next, err := NextSessionGeneration("repo-dir-failure")
	if err != nil {
		t.Fatal(err)
	}
	if next <= 1 {
		t.Fatalf("directory failure permitted generation reuse: %d", next)
	}
}

func TestReceiptDirectoryDurabilityFailureIsObservable(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	restore := setDirectorySyncForTest(func(string) error { return errors.New("injected receipt directory fsync failure") })
	err := (&JSONLSink{Path: path}).Write(Receipt{Action: "owner", Identity: owner()})
	restore()
	if err == nil {
		t.Fatal("receipt write returned before directory durability")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("receipt bytes were not retained: %v", statErr)
	}
}

func TestLifecycleReadersRejectUnframedFinalRecord(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	ow := owner()
	ow.Repository, ow.TabID, ow.PaneID, ow.SessionID, ow.TaskRef, ow.Lane = "repo", "tab", "pane", "session", "task", "lane"
	b, err := json.Marshal(Receipt{Action: "owner", Identity: ow})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLifecycle(path, "tab", &FakeTree{}, &JSONLSink{Path: path}); err == nil {
		t.Fatal("complete but unframed final record became cleanup authority")
	}
}

func TestVerifyTerminalRejectsBlankFrame(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	ow := owner()
	ow.Repository, ow.TabID, ow.PaneID, ow.SessionID, ow.TaskRef, ow.Lane = "repo", "tab", "pane", "session", "task", "lane"
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(Receipt{Action: "tombstone", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := NewLifecycle(ow, &FakeTree{}, sink).VerifyTerminal(); err == nil {
		t.Fatal("blank receipt frame became terminal proof")
	}
}

func TestFakeTreeReapRevalidatesStartTokenAtActionBoundary(t *testing.T) {
	expected := child(20)
	tree := &FakeTree{Nodes: map[int]Node{20: {Identity: Identity{PID: 20, StartToken: "reused"}}}}
	if err := tree.Reap(expected); err == nil {
		t.Fatal("PID reuse crossed identity-bearing reap boundary")
	}
	if len(tree.Reaped) != 0 {
		t.Fatal("rejected reap mutated fake tree")
	}
}

func TestRecoveryRescansOwnerAndResumesPendingIntentPhase(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	ow := owner()
	ow.Repository, ow.TabID, ow.PaneID, ow.SessionID, ow.TaskRef, ow.Lane = "repo", "tab", "pane", "session", "task", "lane"
	childID := child(20)
	childID.Repository, childID.TabID, childID.PaneID, childID.SessionID, childID.TaskRef, childID.Lane = "repo", "tab", "pane", "session", "task", "lane"
	childID.OwnerPID, childID.OwnerStartToken = ow.PID, ow.StartToken
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(Receipt{Action: "inventory", Identity: childID}); err != nil {
		t.Fatal(err)
	}
	withoutIntent, err := LoadLifecycle(path, "tab", &FakeTree{Nodes: map[int]Node{10: {Identity: ow}, 20: {Identity: childID, ParentPID: 10}}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if withoutIntent.RecoveredPhase != 2 {
		t.Fatalf("phase=%d want 2", withoutIntent.RecoveredPhase)
	}
	if err := withoutIntent.Reconcile("recovery"); err != nil {
		t.Fatal(err)
	}

	// A fresh phase-3 history must not call Begin, which would append an
	// inventory transition after an intent. The exact child is already absent,
	// so idempotent teardown completes the recorded intent.
	path2 := t.TempDir() + "/receipts.jsonl"
	sink2 := &JSONLSink{Path: path2}
	childTwo := child(21)
	childTwo.Repository, childTwo.TabID, childTwo.PaneID, childTwo.SessionID, childTwo.TaskRef, childTwo.Lane = "repo", "tab", "pane", "session", "task", "lane"
	childTwo.OwnerPID, childTwo.OwnerStartToken = ow.PID, ow.StartToken
	for _, r := range []Receipt{{Action: "owner", Identity: ow}, {Action: "inventory", Identity: childID}, {Action: "inventory", Identity: childTwo}, {Action: "reap-intent", Identity: childID}} {
		if err := sink2.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	resumed, err := LoadLifecycle(path2, "tab", &FakeTree{Nodes: map[int]Node{}}, sink2)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RecoveredPhase != 3 {
		t.Fatalf("phase=%d want 3", resumed.RecoveredPhase)
	}
	if err := resumed.Reconcile("recovery"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	ownerRecords := 0
	teardownRecords := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var receipt Receipt
		if err := json.Unmarshal([]byte(line), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Action == "owner" {
			ownerRecords++
		}
		if receipt.Action == "teardown" { teardownRecords++ }
	}
	if ownerRecords != 1 {
		t.Fatalf("phase-3 recovery restarted Begin: owner records=%d", ownerRecords)
	}
	if teardownRecords != 2 { t.Fatalf("phase-3 recovery left known children pending: teardown records=%d", teardownRecords) }
}

type failingTree struct {
	*FakeTree
	enumErr, reapErr error
}

type failAfterSink struct {
	inner         *JSONLSink
	count, failAt int
}

func (s *failAfterSink) Write(r Receipt) error {
	s.count++
	if s.count == s.failAt {
		return errors.New("injected receipt write failure")
	}
	return s.inner.Write(r)
}

func (f *failingTree) Descendants(int) ([]Node, error) {
	if f.enumErr != nil {
		return nil, f.enumErr
	}
	return f.FakeTree.Descendants(10)
}
func (f *failingTree) Lookup(pid int) (Node, bool, error) { return f.FakeTree.Lookup(pid) }
func (f *failingTree) Reap(expected Identity) error {
	if f.reapErr != nil {
		return f.reapErr
	}
	return f.FakeTree.Reap(expected)
}

func owner() Identity {
	return Identity{PID: 10, StartToken: "owner-start", SessionGeneration: 7, LaunchID: "launch-a", Repository: "repo", Role: "forge-smith", Lane: "lane-a"}
}
func child(pid int) Identity {
	return Identity{PID: pid, ParentPID: 10, StartToken: "child-start", SessionGeneration: 7, LaunchID: "launch-a", Repository: "repo", Role: "forge-smith", Lane: "lane-a", Server: "code-review-graph", Transport: "stdio"}
}

func TestExactTeardownAndReconciliationUseFakeTreeOnly(t *testing.T) {
	f := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: child(20), ParentPID: 10}, 21: {Identity: child(21), ParentPID: 10}}}
	i := &Inventory{Owner: owner()}
	if err := i.Add(child(20)); err != nil {
		t.Fatal(err)
	}
	r, err := Teardown(f, i, 20)
	if err != nil || !r.Reaped || len(f.Reaped) != 1 || f.Reaped[0] != 20 {
		t.Fatalf("exact teardown failed: %+v %v", r, err)
	}
	if _, err := Reconcile(f, i, "tab-close"); err != nil {
		t.Fatal(err)
	}
	if len(f.Reaped) != 1 {
		t.Fatalf("unowned fake child was reaped: %v", f.Reaped)
	}
}

func TestDifferentLaneAndPIDReuseRefuse(t *testing.T) {
	i := &Inventory{Owner: owner()}
	if err := i.Add(child(20)); err != nil {
		t.Fatal(err)
	}
	other := child(20)
	other.Lane = "lane-b"
	f := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: other, ParentPID: 10}}}
	if _, err := Teardown(f, i, 20); err == nil {
		t.Fatal("different lane ownership must refuse")
	}
	reused := child(20)
	reused.StartToken = "new-start"
	f.Nodes[20] = Node{Identity: reused, ParentPID: 10}
	if _, err := Teardown(f, i, 20); err == nil {
		t.Fatal("PID reuse must refuse")
	}
	if len(f.Reaped) != 0 {
		t.Fatal("refused teardown reaped a child")
	}
}

func TestReconcileEventsAreBounded(t *testing.T) {
	for _, event := range []string{"done", "failed-launch", "recovery", "tab-close"} {
		f := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: child(20), ParentPID: 10}}}
		i := &Inventory{Owner: owner()}
		_ = i.Add(child(20))
		if got, err := Reconcile(f, i, event); err != nil || len(got) != 1 || !got[0].Reaped {
			t.Fatalf("%s: %+v %v", event, got, err)
		}
	}
	if _, err := Reconcile(&FakeTree{}, &Inventory{Owner: owner()}, "unknown"); err == nil {
		t.Fatal("unknown event must fail closed")
	}
}

func TestLifecycleRefreshesLateDescendantsBeforeReconcile(t *testing.T) {
	f := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: child(20), ParentPID: 10}}}
	l := NewLifecycle(owner(), f, &MemorySink{})
	if err := l.Begin(); err != nil {
		t.Fatal(err)
	}
	f.Nodes[21] = Node{Identity: child(21), ParentPID: 20}
	if err := l.Reconcile("recovery"); err != nil {
		t.Fatal(err)
	}
	if len(f.Reaped) != 2 {
		t.Fatalf("late descendant was not reconciled: %v", f.Reaped)
	}
}

func TestLifecycleFailsClosedOnEnumerationAndReapVerificationErrors(t *testing.T) {
	base := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: child(20), ParentPID: 10}}}
	if err := NewLifecycle(owner(), &failingTree{FakeTree: base, enumErr: errors.New("enumeration failed")}, &MemorySink{}).Begin(); err == nil {
		t.Fatal("enumeration error became empty inventory")
	}
	base = &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: child(20), ParentPID: 10}}}
	l := NewLifecycle(owner(), &failingTree{FakeTree: base, reapErr: errors.New("wait/readback failed")}, &MemorySink{})
	if err := l.Reconcile("done"); err == nil {
		t.Fatal("reap verification failure was accepted")
	}
	if len(base.Reaped) != 0 {
		t.Fatal("failed reap mutated fake tree")
	}
}

func TestVerifyTerminalRequiresLatestIdentityFencedTombstone(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	ow := owner()
	ow.TabID, ow.PaneID, ow.SessionID, ow.Repository = "tab", "pane", "session", "repo"
	l := NewLifecycle(ow, &FakeTree{Nodes: map[int]Node{10: {Identity: ow}}}, sink)
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(Receipt{Action: "tombstone", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	c := child(20)
	c.TabID, c.PaneID, c.SessionID, c.Repository = "tab", "pane", "session", "repo"
	c.OwnerPID, c.OwnerStartToken = ow.PID, ow.StartToken
	if err := sink.Write(Receipt{Action: "inventory", Identity: c}); err != nil {
		t.Fatal(err)
	}
	if err := l.VerifyTerminal(); err == nil {
		t.Fatal("historical tombstone incorrectly authorized terminal state")
	}
}

func TestVerifyTerminalRejectsSameTabMismatchedGeneration(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	ow := owner()
	ow.TabID, ow.PaneID, ow.SessionID, ow.Repository, ow.TaskRef, ow.Lane = "tab-live", "pane-live", "session-live", "repo-live", "task-live", "lane-live"
	l := NewLifecycle(ow, &FakeTree{}, sink)
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	foreign := ow
	foreign.PaneID = "pane-foreign"
	foreign.SessionID = "session-foreign"
	foreign.Repository = "repo-foreign"
	foreign.SessionGeneration++
	foreign.LaunchID = "launch-foreign"
	foreign.PID++
	foreign.StartToken = "start-foreign"
	if err := sink.Write(Receipt{Action: "tombstone", Identity: foreign}); err != nil {
		t.Fatal(err)
	}
	if err := l.VerifyTerminal(); err == nil {
		t.Fatal("mismatched same-tab generation became terminal authority")
	}
}

func TestBeginFailsClosedWhenDescendantIdentityCannotBeAdded(t *testing.T) {
	f := &FakeTree{Nodes: map[int]Node{10: {Identity: owner()}, 20: {Identity: Identity{PID: 20}, ParentPID: 10}}}
	if err := NewLifecycle(owner(), f, &MemorySink{}).Begin(); err == nil {
		t.Fatal("invalid discovered descendant was silently discarded")
	}
}

func TestLoadLifecycleRejectsUnknownAndOutOfOrderTransitions(t *testing.T) {
	for _, action := range []string{"unknown", "tombstone", "teardown"} {
		t.Run(action, func(t *testing.T) {
			path := t.TempDir() + "/receipts.jsonl"
			ow := owner()
			ow.TabID, ow.PaneID, ow.Repository, ow.TaskRef, ow.Lane = "tab", "pane", "repo", "task", "lane"
			if err := (&JSONLSink{Path: path}).Write(Receipt{Action: action, Identity: ow, Reaped: action == "teardown"}); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadLifecycle(path, "tab", &FakeTree{}, &JSONLSink{Path: path}); err == nil {
				t.Fatalf("%s before owner was accepted", action)
			}
		})
	}
}

func TestLoadLifecycleRejectsTeardownWithoutExactChildIntent(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	ow := owner()
	ow.SessionID = "session"
	ow.TabID, ow.PaneID, ow.Repository, ow.TaskRef, ow.Lane = "tab", "pane", "repo", "task", "lane"
	if err := sink.Write(Receipt{Action: "owner", Identity: ow}); err != nil {
		t.Fatal(err)
	}
	childID := child(20)
	childID.TabID, childID.PaneID, childID.Repository, childID.TaskRef, childID.Lane = "tab", "pane", "repo", "task", "lane"
	childID.SessionID = "session"
	childID.SessionID = "session"
	childID.OwnerPID, childID.OwnerStartToken = ow.PID, ow.StartToken
	if err := sink.Write(Receipt{Action: "inventory", Identity: childID}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(Receipt{Action: "teardown", Identity: childID, Reaped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLifecycle(path, "tab", &FakeTree{}, sink); err == nil {
		t.Fatal("teardown without exact reap intent was accepted")
	}
}

func TestRestartReconcilesAlreadyAbsentChildAfterResultWriteFailure(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	ow := owner()
	ow.SessionID = "session"
	ow.TabID, ow.PaneID, ow.Repository, ow.TaskRef, ow.Lane = "tab", "pane", "repo", "task", "lane"
	childID := child(20)
	childID.TabID, childID.PaneID, childID.Repository, childID.TaskRef, childID.Lane = "tab", "pane", "repo", "task", "lane"
	childID.SessionID = "session"
	childID.OwnerPID, childID.OwnerStartToken = ow.PID, ow.StartToken
	tree := &FakeTree{Nodes: map[int]Node{10: {Identity: ow}, 20: {Identity: childID, ParentPID: 10}}}
	base := &JSONLSink{Path: path}
	l := NewLifecycle(ow, tree, &failAfterSink{inner: base, failAt: 4})
	if err := l.Inventory.Add(childID); err != nil {
		t.Fatal(err)
	}
	if err := l.Reconcile("done"); err == nil {
		t.Fatal("result write failure was accepted")
	}
	if len(tree.Reaped) != 1 {
		t.Fatalf("child was not reaped before injected receipt failure: %v", tree.Reaped)
	}
	recovered, err := LoadLifecycle(path, "tab", tree, base)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("%v receipts=%s", err, b)
	}
	if err := recovered.Reconcile("recovery"); err != nil {
		t.Fatalf("restart did not converge on absent exact generation: %v", err)
	}
}

func TestLoadLifecycleUsesLatestExactGenerationAndProvisionalAuthority(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	old := owner()
	old.TabID, old.PaneID, old.Repository, old.TaskRef, old.SessionID = "tab", "pane", "repo", "FAC-1", "s1"
	newOwner := old
	newOwner.LaunchID = "launch-new"
	newOwner.SessionGeneration = 8
	newOwner.SessionID = "s2"
	newOwner.PID = 0
	newOwner.StartToken = ""
	for _, r := range []Receipt{{Action: "owner", Identity: old}, {Action: "tombstone", Identity: old}, {Action: "provisional", Identity: newOwner}} {
		if err := sink.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	l, err := LoadLifecycle(path, "tab", &FakeTree{Nodes: map[int]Node{}}, &JSONLSink{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if l.Inventory.Owner.LaunchID != "launch-new" || l.Bound() {
		t.Fatalf("wrong reconstructed generation: %+v", l.Inventory.Owner)
	}
}
