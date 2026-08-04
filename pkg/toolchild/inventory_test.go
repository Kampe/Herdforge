package toolchild

import (
	"errors"
	"testing"
)

type failingTree struct {
	*FakeTree
	enumErr, reapErr error
}

func (f *failingTree) Descendants(int) ([]Node, error) {
	if f.enumErr != nil {
		return nil, f.enumErr
	}
	return f.FakeTree.Descendants(10)
}
func (f *failingTree) Lookup(pid int) (Node, bool, error) { return f.FakeTree.Lookup(pid) }
func (f *failingTree) Reap(pid int) error {
	if f.reapErr != nil {
		return f.reapErr
	}
	return f.FakeTree.Reap(pid)
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
