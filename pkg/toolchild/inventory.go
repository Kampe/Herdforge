// Package toolchild models lane-owned tool children without inspecting or
// signalling host processes. Production adapters may implement the Tree
// interface; tests use only FakeTree.
package toolchild

import (
	"errors"
	"fmt"
	"sort"
)

var (
	ErrNotOwned       = errors.New("tool child is not proven lane-owned")
	ErrUnsafeTeardown = errors.New("unsafe tool-child teardown refused")
)

type Identity struct {
	PID               int    `json:"pid"`
	ParentPID         int    `json:"parent_pid"`
	StartToken        string `json:"start_token"`
	SessionGeneration int64  `json:"session_generation"`
	LaunchID          string `json:"launch_id"`
	Repository        string `json:"repository"`
	Role              string `json:"role"`
	Lane              string `json:"lane"`
	Server            string `json:"server"`
	Transport         string `json:"transport"`
}

type Node struct {
	Identity  Identity
	ParentPID int
	Children  []int
}

type Receipt struct {
	Action   string   `json:"action"`
	Identity Identity `json:"identity"`
	Reaped   bool     `json:"reaped"`
	Reason   string   `json:"reason,omitempty"`
}

type Tree interface {
	Lookup(pid int) (Node, bool)
	Reap(pid int) error
}

type FakeTree struct {
	Nodes  map[int]Node
	Reaped []int
}

func (f *FakeTree) Lookup(pid int) (Node, bool) { n, ok := f.Nodes[pid]; return n, ok }
func (f *FakeTree) Reap(pid int) error {
	if _, ok := f.Nodes[pid]; !ok {
		return errors.New("fake child missing")
	}
	delete(f.Nodes, pid)
	f.Reaped = append(f.Reaped, pid)
	return nil
}

type Inventory struct {
	Owner    Identity
	Children []Identity
	Receipts []Receipt
}

func (i *Inventory) Add(child Identity) error {
	if i == nil || child.PID <= 0 || child.StartToken == "" || child.LaunchID == "" || child.SessionGeneration <= 0 {
		return ErrNotOwned
	}
	if len(i.Children) > 0 && i.Children[0].Lane != child.Lane {
		return ErrNotOwned
	}
	i.Children = append(i.Children, child)
	sort.Slice(i.Children, func(a, b int) bool { return i.Children[a].PID < i.Children[b].PID })
	return nil
}

func prove(tree Tree, i Inventory, c Identity) error {
	if i.Owner.PID <= 0 || i.Owner.StartToken == "" || i.Owner.LaunchID == "" || i.Owner.SessionGeneration <= 0 {
		return ErrNotOwned
	}
	if c.PID <= 0 || c.StartToken == "" || c.LaunchID == "" {
		return ErrNotOwned
	}
	if c.LaunchID != i.Owner.LaunchID || c.SessionGeneration != i.Owner.SessionGeneration || c.Repository != i.Owner.Repository || c.Role != i.Owner.Role || c.Lane != i.Owner.Lane {
		return ErrNotOwned
	}
	// Walk the live parent chain. A recorded parent PID alone is insufficient
	// because PID reuse can make an unrelated process look like the owner.
	seen := map[int]bool{c.PID: true}
	parent := c.ParentPID
	for depth := 0; depth < 64; depth++ {
		if parent == i.Owner.PID {
			n, ok := tree.Lookup(parent)
			if !ok || n.Identity.StartToken != i.Owner.StartToken || n.Identity.LaunchID != i.Owner.LaunchID || n.Identity.SessionGeneration != i.Owner.SessionGeneration {
				return ErrNotOwned
			}
			return nil
		}
		if parent <= 0 || seen[parent] {
			return ErrNotOwned
		}
		seen[parent] = true
		n, ok := tree.Lookup(parent)
		if !ok {
			return ErrNotOwned
		}
		parent = n.ParentPID
	}
	return nil
}

// Teardown reaps only the exact recorded child after revalidating the current
// fake tree identity. No names, globs, process groups, or broad PID actions.
func Teardown(tree Tree, i *Inventory, pid int) (Receipt, error) {
	r := Receipt{Action: "teardown", Identity: Identity{PID: pid}}
	if tree == nil || i == nil {
		r.Reason = "missing tree or inventory"
		return r, ErrUnsafeTeardown
	}
	var expected *Identity
	for n := range i.Children {
		if i.Children[n].PID == pid {
			expected = &i.Children[n]
			break
		}
	}
	if expected == nil {
		r.Reason = "pid is not in exact lane inventory"
		return r, ErrUnsafeTeardown
	}
	n, ok := tree.Lookup(pid)
	if !ok || n.Identity != *expected {
		r.Reason = "pid reuse, changed parent, or identity mismatch"
		return r, ErrUnsafeTeardown
	}
	if err := prove(tree, *i, n.Identity); err != nil {
		r.Reason = err.Error()
		return r, err
	}
	if err := tree.Reap(pid); err != nil {
		r.Reason = err.Error()
		return r, fmt.Errorf("reap owned child: %w", err)
	}
	r.Identity = n.Identity
	r.Reaped = true
	i.Receipts = append(i.Receipts, r)
	for n := range i.Children {
		if i.Children[n].PID == pid {
			i.Children = append(i.Children[:n], i.Children[n+1:]...)
			break
		}
	}
	return r, nil
}

func Reconcile(tree Tree, i *Inventory, event string) ([]Receipt, error) {
	if event != "done" && event != "failed-launch" && event != "recovery" && event != "tab-close" {
		return nil, ErrUnsafeTeardown
	}
	var out []Receipt
	for _, c := range append([]Identity(nil), i.Children...) {
		r, err := Teardown(tree, i, c.PID)
		out = append(out, r)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
