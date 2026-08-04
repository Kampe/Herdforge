package toolchild

import "testing"

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
