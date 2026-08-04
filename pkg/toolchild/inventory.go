// Package toolchild models lane-owned tool children without inspecting or
// signalling host processes. Production adapters may implement the Tree
// interface; tests use only FakeTree.
package toolchild

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
	SessionID         string `json:"session_id"`
	PaneID            string `json:"pane_id"`
	Provider          string `json:"provider"`
	ArgvDigest        string `json:"argv_digest"`
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
	Lookup(pid int) (Node, bool, error)
	Reap(pid int) error
}

// DescendantTree is implemented by production and fake trees that can
// enumerate children without broad process matching.
type DescendantTree interface {
	Tree
	Descendants(pid int) ([]Node, error)
}

type FakeTree struct {
	Nodes  map[int]Node
	Reaped []int
}

func (f *FakeTree) Lookup(pid int) (Node, bool, error) { n, ok := f.Nodes[pid]; return n, ok, nil }
func (f *FakeTree) Reap(pid int) error {
	if _, ok := f.Nodes[pid]; !ok {
		return errors.New("fake child missing")
	}
	delete(f.Nodes, pid)
	f.Reaped = append(f.Reaped, pid)
	return nil
}
func (f *FakeTree) Descendants(pid int) ([]Node, error) {
	var out []Node
	queue := []int{pid}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, n := range f.Nodes {
			if n.ParentPID == parent || n.Identity.ParentPID == parent {
				out = append(out, n)
				queue = append(queue, n.Identity.PID)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Identity.PID < out[b].Identity.PID })
	return out, nil
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
	for _, existing := range i.Children {
		if existing.PID == child.PID {
			if existing == child {
				return nil
			}
			return ErrNotOwned
		}
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
	if c.LaunchID != i.Owner.LaunchID || c.SessionGeneration != i.Owner.SessionGeneration || c.Repository != i.Owner.Repository || c.Role != i.Owner.Role || c.Lane != i.Owner.Lane || c.SessionID != i.Owner.SessionID || c.PaneID != i.Owner.PaneID || c.Provider != i.Owner.Provider || c.ArgvDigest != i.Owner.ArgvDigest {
		return ErrNotOwned
	}
	// Walk the live parent chain. A recorded parent PID alone is insufficient
	// because PID reuse can make an unrelated process look like the owner.
	seen := map[int]bool{c.PID: true}
	parent := c.ParentPID
	for depth := 0; depth < 64; depth++ {
		if parent == i.Owner.PID {
			n, ok, lookupErr := tree.Lookup(parent)
			if lookupErr != nil {
				return lookupErr
			}
			if !ok || n.Identity.PID != i.Owner.PID || n.Identity.StartToken != i.Owner.StartToken {
				return ErrNotOwned
			}
			return nil
		}
		if parent <= 0 || seen[parent] {
			return ErrNotOwned
		}
		seen[parent] = true
		n, ok, lookupErr := tree.Lookup(parent)
		if lookupErr != nil {
			return lookupErr
		}
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
	n, ok, lookupErr := tree.Lookup(pid)
	if lookupErr != nil {
		r.Reason = lookupErr.Error()
		return r, lookupErr
	}
	observed := *expected
	observed.ParentPID = n.Identity.ParentPID
	if observed.ParentPID == 0 {
		observed.ParentPID = n.ParentPID
	}
	observed.StartToken = n.Identity.StartToken
	metadataMismatch := n.Identity.LaunchID != "" && (n.Identity.LaunchID != expected.LaunchID || n.Identity.SessionGeneration != expected.SessionGeneration || n.Identity.Repository != expected.Repository || n.Identity.Role != expected.Role || n.Identity.Lane != expected.Lane || n.Identity.SessionID != expected.SessionID || n.Identity.PaneID != expected.PaneID || n.Identity.Provider != expected.Provider || n.Identity.ArgvDigest != expected.ArgvDigest)
	if !ok || metadataMismatch || n.Identity.PID != expected.PID || observed.ParentPID != expected.ParentPID || observed.StartToken != expected.StartToken {
		r.Reason = "pid reuse, changed parent, or identity mismatch"
		return r, ErrUnsafeTeardown
	}
	proofIdentity := *expected
	proofIdentity.ParentPID = observed.ParentPID
	proofIdentity.StartToken = observed.StartToken
	if err := prove(tree, *i, proofIdentity); err != nil {
		r.Reason = err.Error()
		return r, err
	}
	if err := tree.Reap(pid); err != nil {
		r.Reason = err.Error()
		return r, fmt.Errorf("reap owned child: %w", err)
	}
	r.Identity = *expected
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

// ReceiptSink is the durable evidence port for inventory and teardown.
type ReceiptSink interface{ Write(Receipt) error }

type JSONLSink struct {
	Path string
	mu   sync.Mutex
}

func (s *JSONLSink) Write(r Receipt) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("tool-child receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepathDir(s.Path), 0755); err != nil {
		return fmt.Errorf("create tool-child receipt directory: %w", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open tool-child receipt: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal tool-child receipt: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write tool-child receipt: %w", err)
	}
	return f.Sync()
}

// filepathDir is kept local so the package has one narrow filesystem seam.
func filepathDir(path string) string {
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		return path[:i]
	}
	return "."
}

type MemorySink struct{ Receipts []Receipt }

func (s *MemorySink) Write(r Receipt) error { s.Receipts = append(s.Receipts, r); return nil }

// Lifecycle is the production-owned inventory/reconcile adapter. Begin and
// Reconcile both fail closed when evidence cannot be persisted.
type Lifecycle struct {
	Tree      DescendantTree
	Sink      ReceiptSink
	Inventory Inventory
}

func NewLifecycle(owner Identity, tree DescendantTree, sink ReceiptSink) *Lifecycle {
	return &Lifecycle{Tree: tree, Sink: sink, Inventory: Inventory{Owner: owner}}
}

func (l *Lifecycle) Bind(owner Identity) error {
	if l == nil || owner.PID <= 0 || owner.StartToken == "" || owner.LaunchID == "" || owner.SessionGeneration <= 0 {
		return ErrNotOwned
	}
	l.Inventory.Owner = owner
	return nil
}

func (l *Lifecycle) Begin() error {
	if l == nil || l.Tree == nil || l.Sink == nil || l.Inventory.Owner.PID <= 0 || l.Inventory.Owner.StartToken == "" || l.Inventory.Owner.LaunchID == "" || l.Inventory.Owner.SessionGeneration <= 0 {
		return ErrUnsafeTeardown
	}
	nodes, err := l.Tree.Descendants(l.Inventory.Owner.PID)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		n.Identity.LaunchID = l.Inventory.Owner.LaunchID
		n.Identity.SessionGeneration = l.Inventory.Owner.SessionGeneration
		n.Identity.Repository = l.Inventory.Owner.Repository
		n.Identity.Role = l.Inventory.Owner.Role
		n.Identity.Lane = l.Inventory.Owner.Lane
		n.Identity.SessionID = l.Inventory.Owner.SessionID
		n.Identity.PaneID = l.Inventory.Owner.PaneID
		n.Identity.Provider = l.Inventory.Owner.Provider
		n.Identity.ArgvDigest = l.Inventory.Owner.ArgvDigest
		if err := l.Inventory.Add(n.Identity); err != nil {
			continue
		}
		if err := l.Sink.Write(Receipt{Action: "inventory", Identity: n.Identity, Reason: "launch child captured"}); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lifecycle) Reconcile(event string) error {
	if l == nil || l.Sink == nil {
		return ErrUnsafeTeardown
	}
	if err := l.Begin(); err != nil {
		return err
	}
	receipts, err := Reconcile(l.Tree, &l.Inventory, event)
	for _, r := range receipts {
		if werr := l.Sink.Write(r); werr != nil {
			return werr
		}
	}
	return err
}

// SystemTree is the production adapter. It uses exact PIDs and parent chains;
// it never searches by name, group, or a broad process pattern.
type SystemTree struct{}

func (SystemTree) Lookup(pid int) (Node, bool, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=,lstart=").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return Node{}, false, nil
		}
		return Node{}, false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return Node{}, false, errors.New("process lookup returned incomplete identity")
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil {
		return Node{}, false, err
	}
	return Node{Identity: Identity{PID: pid, ParentPID: ppid, StartToken: strings.Join(fields[1:], " ")}, ParentPID: ppid}, true, nil
}
func (s SystemTree) Descendants(pid int) ([]Node, error) {
	queue := []int{pid}
	var nodes []Node
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				continue
			}
			return nil, err
		}
		for _, raw := range strings.Fields(string(out)) {
			child, err := strconv.Atoi(raw)
			if err != nil {
				return nil, err
			}
			n, ok, err := s.Lookup(child)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("process disappeared during enumeration")
			}
			nodes = append(nodes, n)
			queue = append(queue, child)
		}
	}
	return nodes, nil
}
func (SystemTree) Reap(pid int) error {
	if pid <= 0 {
		return ErrUnsafeTeardown
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, err := (SystemTree{}).Lookup(pid)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("exact child did not exit before teardown verification deadline")
}
