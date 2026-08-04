// Package toolchild models lane-owned tool children without inspecting or
// signalling host processes. Production adapters may implement the Tree
// interface; tests use only FakeTree.
package toolchild

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RepositoryIdentity derives a non-secret canonical origin binding from git's
// configured remote. It is an identity binding, not cryptographic proof of
// remote ownership; callers must not describe it as authentication.
func RepositoryIdentity(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrUnsafeTeardown
	}
	out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", fmt.Errorf("authenticated repository origin: %w", err)
	}
	origin := strings.TrimSpace(string(out))
	if origin == "" {
		return "", fmt.Errorf("authenticated repository origin is empty")
	}
	if strings.Contains(origin, "://") {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			return strings.ToLower(u.Host) + "/" + strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), nil
		}
	}
	if at := strings.Index(origin, ":"); at > 0 && !strings.Contains(origin[:at], "/") {
		// SCP-style git@host:path, normalized to the same host/path as URLs.
		host := origin[:at]
		if user := strings.LastIndex(host, "@"); user >= 0 {
			host = host[user+1:]
		}
		return strings.ToLower(host) + "/" + strings.Trim(strings.TrimSuffix(origin[at+1:], ".git"), "/"), nil
	}
	local := origin
	if strings.HasPrefix(local, "file://") {
		local = strings.TrimPrefix(local, "file://")
	}
	if !filepath.IsAbs(local) {
		local = filepath.Join(root, local)
	}
	abs, err := filepath.Abs(local)
	if err != nil {
		return "", err
	}
	return "file/" + filepath.Clean(abs), nil
}

var (
	ErrNotOwned           = errors.New("tool child is not proven lane-owned")
	ErrUnsafeTeardown     = errors.New("unsafe tool-child teardown refused")
	ErrLifecycleNotFound  = errors.New("no exact lifecycle for tab")
	ErrLifecycleCollision = errors.New("tool-child lifecycle identity collision")
)

type Identity struct {
	PID               int      `json:"pid"`
	ParentPID         int      `json:"parent_pid"`
	StartToken        string   `json:"start_token"`
	SessionGeneration int64    `json:"session_generation"`
	LaunchID          string   `json:"launch_id"`
	Repository        string   `json:"repository"`
	Role              string   `json:"role"`
	Lane              string   `json:"lane"`
	Server            string   `json:"server"`
	Transport         string   `json:"transport"`
	SessionID         string   `json:"session_id"`
	PaneID            string   `json:"pane_id"`
	TabID             string   `json:"tab_id"`
	Provider          string   `json:"provider"`
	ArgvDigest        string   `json:"argv_digest"`
	Argv              []string `json:"argv,omitempty"`
	TaskRef           string   `json:"task_ref"`
	Name              string   `json:"name,omitempty"`
	OwnerPID          int      `json:"owner_pid,omitempty"`
	OwnerStartToken   string   `json:"owner_start_token,omitempty"`
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
			if reflect.DeepEqual(existing, child) {
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
	if c.LaunchID != i.Owner.LaunchID || c.SessionGeneration != i.Owner.SessionGeneration || c.Repository != i.Owner.Repository || c.Role != i.Owner.Role || c.Lane != i.Owner.Lane || c.SessionID != i.Owner.SessionID || c.PaneID != i.Owner.PaneID || c.TabID != i.Owner.TabID || c.Provider != i.Owner.Provider || c.ArgvDigest != i.Owner.ArgvDigest || c.TaskRef != i.Owner.TaskRef {
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
	if !ok {
		// A previously verified exact generation may already be absent after a
		// coordinator crash. Absence is a successful idempotent terminal read;
		// a reused PID cannot match because the inventory retains its start token.
		r.Identity = *expected
		r.Reaped = true
		r.Reason = "exact recorded generation already absent"
		i.Receipts = append(i.Receipts, r)
		for n := range i.Children {
			if i.Children[n].PID == pid && i.Children[n].StartToken == expected.StartToken {
				i.Children = append(i.Children[:n], i.Children[n+1:]...)
				break
			}
		}
		return r, nil
	}
	observed := *expected
	observed.ParentPID = n.Identity.ParentPID
	if observed.ParentPID == 0 {
		observed.ParentPID = n.ParentPID
	}
	observed.StartToken = n.Identity.StartToken
	metadataMismatch := n.Identity.LaunchID != "" && (n.Identity.LaunchID != expected.LaunchID || n.Identity.SessionGeneration != expected.SessionGeneration || n.Identity.Repository != expected.Repository || n.Identity.Role != expected.Role || n.Identity.Lane != expected.Lane || n.Identity.SessionID != expected.SessionID || n.Identity.PaneID != expected.PaneID || n.Identity.TabID != expected.TabID || n.Identity.Provider != expected.Provider || n.Identity.ArgvDigest != expected.ArgvDigest || n.Identity.TaskRef != expected.TaskRef)
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

func StableReceiptPath(repository string) (string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return "", ErrUnsafeTeardown
	}
	root := os.Getenv("HERD_TOOLCHILD_RECEIPT_ROOT")
	if root == "" {
		cache, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(cache, "Herdforge", "toolchild")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(repository)))
	return filepath.Join(root, digest+".jsonl"), nil
}

// NextSessionGeneration derives the next generation from durable lifecycle
// history, so restart recovery does not depend on a process-local clock.
func NextSessionGeneration(repository string) (result int64, err error) {
	path, err := StableReceiptPath(repository)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepathDir(path), 0700); err != nil {
		return 0, err
	}
	lock, err := lockReceipt(path)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, releaseReceiptLock(lock)) }()
	b, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return 0, readErr
	}
	if readErr == nil {
		if len(b) == 0 || b[len(b)-1] != '\n' {
			return 0, fmt.Errorf("corrupt lifecycle receipt: incomplete final frame")
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				return 0, fmt.Errorf("corrupt lifecycle receipt")
			}
			var r Receipt
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				return 0, err
			}
			if r.Identity.SessionGeneration > result {
				result = r.Identity.SessionGeneration
			}
		}
	}
	result++
	reservation, err := json.Marshal(Receipt{Action: "session-reservation", Identity: Identity{Repository: repository, SessionGeneration: result}})
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	frame := append(reservation, '\n')
	if n, writeErr := f.Write(frame); writeErr != nil || n != len(frame) {
		closeErr := f.Close()
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return 0, errors.Join(writeErr, closeErr, quarantineReceipt(path))
	}
	if err := f.Sync(); err != nil {
		return 0, errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	// Read back while holding the same lock. Returning a generation without
	// this proof would let a concurrent coordinator use an uncommitted value.
	readback, err := os.ReadFile(path)
	if err != nil || len(readback) == 0 || readback[len(readback)-1] != '\n' {
		return 0, errors.Join(err, fmt.Errorf("session reservation readback failed"))
	}
	lines := strings.Split(strings.TrimSuffix(string(readback), "\n"), "\n")
	var last Receipt
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &last) != nil || last.Action != "session-reservation" || last.Identity.Repository != repository || last.Identity.SessionGeneration != result {
		return 0, fmt.Errorf("session reservation readback mismatch")
	}
	return result, nil
}

func quarantineReceipt(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return os.Rename(path, path+".quarantine")
}

func lockReceipt(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func releaseReceiptLock(lock *os.File) error {
	if lock == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
}

func (s *JSONLSink) Write(r Receipt) (err error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("tool-child receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepathDir(s.Path), 0700); err != nil {
		return fmt.Errorf("create tool-child receipt directory: %w", err)
	}
	lock, err := lockReceipt(s.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseReceiptLock(lock)) }()
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open tool-child receipt: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal tool-child receipt: %w", err)
	}
	line := append(b, '\n')
	if n, err := f.Write(line); err != nil {
		return fmt.Errorf("write tool-child receipt: %w", err)
	} else if n != len(line) {
		return errors.Join(io.ErrShortWrite, quarantineReceipt(s.Path))
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
	// RecoveredPhase is the reducer phase reconstructed from durable history.
	// Phase 3 is an in-flight teardown and must resume its exact intents;
	// earlier phases rescan the exact owner ancestry for late children.
	RecoveredPhase int
	PendingIntents map[string]bool
}

func lifecycleKey(i Identity) string {
	ownerPID, ownerToken := i.OwnerPID, i.OwnerStartToken
	if ownerPID == 0 {
		ownerPID, ownerToken = i.PID, i.StartToken
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s|%s|%s|%s|%s|%d|%s", i.Repository, i.TaskRef, i.Lane, i.SessionGeneration, i.TabID, i.PaneID, i.SessionID, i.LaunchID, i.Role, i.Provider, i.ArgvDigest, ownerPID, ownerToken)
}

func childGenerationKey(i Identity) string {
	return fmt.Sprintf("%d:%s", i.PID, i.StartToken)
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
func (l *Lifecycle) Bound() bool {
	return l != nil && l.Inventory.Owner.PID > 0 && l.Inventory.Owner.StartToken != ""
}

func (l *Lifecycle) SetContext(context Identity) {
	if l != nil {
		l.Inventory.Owner = context
	}
}
func (l *Lifecycle) Provision() error {
	if l == nil || l.Sink == nil || l.Inventory.Owner.TabID == "" || l.Inventory.Owner.PaneID == "" || l.Inventory.Owner.LaunchID == "" {
		return ErrUnsafeTeardown
	}
	return l.Sink.Write(Receipt{Action: "provisional", Identity: l.Inventory.Owner, Reason: "prepared tab authority"})
}
func (l *Lifecycle) Invalidate(reason string) error {
	if l == nil || l.Sink == nil || l.Inventory.Owner.TabID == "" {
		return ErrUnsafeTeardown
	}
	return l.Sink.Write(Receipt{Action: "tombstone", Identity: l.Inventory.Owner, Reason: reason})
}

func (l *Lifecycle) VerifyTerminal() (err error) {
	js, ok := l.Sink.(*JSONLSink)
	if !ok || js.Path == "" {
		return ErrUnsafeTeardown
	}
	lock, err := lockReceipt(js.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseReceiptLock(lock)) }()
	b, err := os.ReadFile(js.Path)
	if err != nil {
		return err
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return fmt.Errorf("corrupt lifecycle receipt: incomplete final frame")
	}
	latest := ""
	ownerSeen := false
	terminal := false
	children := map[string]bool{}
	intents := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			return fmt.Errorf("corrupt lifecycle receipt: blank or truncated record")
		}
		var r Receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return err
		}
		if r.Action == "session-reservation" {
			if r.Identity.Repository == "" || r.Identity.SessionGeneration <= 0 || r.Identity.TabID != "" || r.Identity.PaneID != "" {
				return fmt.Errorf("invalid session reservation")
			}
			continue
		}
		if lifecycleKey(r.Identity) != lifecycleKey(l.Inventory.Owner) || (r.Action == "tombstone" && (r.Identity.PID != l.Inventory.Owner.PID || r.Identity.StartToken != l.Inventory.Owner.StartToken)) {
			continue
		}
		if r.Action != "owner" && r.Action != "provisional" && r.Action != "session-reservation" && r.Action != "inventory" && r.Action != "reap-intent" && r.Action != "teardown" && r.Action != "tombstone" {
			return fmt.Errorf("unknown lifecycle action %q", r.Action)
		}
		childKey := childGenerationKey(r.Identity)
		switch r.Action {
		case "owner", "provisional":
			if terminal {
				return ErrUnsafeTeardown
			}
			ownerSeen = true
		case "inventory":
			if !ownerSeen || r.Reaped || r.Identity.StartToken == "" {
				return ErrUnsafeTeardown
			}
			children[childKey] = true
		case "reap-intent":
			if !ownerSeen || r.Reaped || !children[childKey] {
				return ErrUnsafeTeardown
			}
			intents[childKey] = true
		case "teardown":
			if !ownerSeen || !r.Reaped || !children[childKey] || !intents[childKey] {
				return ErrUnsafeTeardown
			}
			delete(intents, childKey)
			delete(children, childKey)
		case "tombstone":
			if !ownerSeen || r.Reaped || len(children) != 0 {
				return ErrUnsafeTeardown
			}
			terminal = true
		}
		latest = r.Action
	}
	if latest != "tombstone" {
		return ErrUnsafeTeardown
	}
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
	if err := l.Sink.Write(Receipt{Action: "owner", Identity: l.Inventory.Owner, Reason: "exact launch owner bound"}); err != nil {
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
		n.Identity.TabID = l.Inventory.Owner.TabID
		n.Identity.Provider = l.Inventory.Owner.Provider
		n.Identity.ArgvDigest = l.Inventory.Owner.ArgvDigest
		n.Identity.TaskRef = l.Inventory.Owner.TaskRef
		n.Identity.OwnerPID = l.Inventory.Owner.PID
		n.Identity.OwnerStartToken = l.Inventory.Owner.StartToken
		if err := l.Inventory.Add(n.Identity); err != nil {
			return fmt.Errorf("discovered descendant identity rejected: %w", err)
		}
		if err := l.Sink.Write(Receipt{Action: "inventory", Identity: n.Identity, Reason: "launch child captured"}); err != nil {
			return err
		}
	}
	return nil
}

// LoadLifecycle reconstructs authority after the coordinator process exits.
// It rejects malformed or ambiguous records instead of treating absence as a
// successful no-op.
func LoadLifecycle(path, tabID string, tree DescendantTree, sink ReceiptSink) (lifecycle *Lifecycle, err error) {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(tabID) == "" || tree == nil || sink == nil {
		return nil, ErrUnsafeTeardown
	}
	lock, err := lockReceipt(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, releaseReceiptLock(lock)) }()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, fmt.Errorf("corrupt lifecycle receipt: incomplete final frame")
	}
	type state struct {
		owner       *Identity
		children    map[string]Identity
		intents     map[string]bool
		terminal    bool
		provisional bool
		phase       int
	}
	states := map[string]*state{}
	latestKey := ""
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			return nil, fmt.Errorf("corrupt lifecycle receipt: blank or truncated record")
		}
		var r Receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		if r.Action == "session-reservation" {
			if r.Identity.Repository == "" || r.Identity.SessionGeneration <= 0 || r.Identity.TabID != "" || r.Identity.PaneID != "" {
				return nil, fmt.Errorf("invalid session reservation")
			}
			continue
		}
		if r.Action != "owner" && r.Action != "provisional" && r.Action != "inventory" && r.Action != "reap-intent" && r.Action != "teardown" && r.Action != "tombstone" {
			return nil, fmt.Errorf("unknown lifecycle action %q", r.Action)
		}
		if r.Identity.TabID != tabID {
			continue
		}
		key := lifecycleKey(r.Identity)
		s := states[key]
		childKey := childGenerationKey(r.Identity)
		switch r.Action {
		case "owner", "provisional":
			if s == nil {
				s = &state{children: map[string]Identity{}, intents: map[string]bool{}}
				states[key] = s
			}
			if s.terminal || (s.owner != nil && !reflect.DeepEqual(*s.owner, r.Identity)) {
				return nil, ErrUnsafeTeardown
			}
			copy := r.Identity
			s.owner = &copy
			if s.phase < 1 {
				s.phase = 1
			}
			s.provisional = r.Action == "provisional"
			latestKey = key
		case "inventory":
			if r.Reaped || s == nil || s.owner == nil || s.terminal || s.phase < 1 || s.phase >= 3 || r.Identity.StartToken == "" {
				return nil, ErrUnsafeTeardown
			}
			if old, ok := s.children[childKey]; ok && !reflect.DeepEqual(old, r.Identity) {
				return nil, ErrUnsafeTeardown
			}
			s.children[childKey] = r.Identity
			s.phase = 2
		case "reap-intent":
			if r.Reaped || s == nil || s.owner == nil || s.terminal || s.phase < 2 || s.children[childKey].PID == 0 {
				return nil, ErrUnsafeTeardown
			}
			s.intents[childKey] = true
			s.phase = 3
		case "teardown":
			if !r.Reaped || s == nil || s.owner == nil || s.terminal || s.phase < 2 || !s.intents[childKey] {
				return nil, ErrUnsafeTeardown
			}
			delete(s.intents, childKey)
			delete(s.children, childKey)
			s.phase = 3
		case "tombstone":
			if s == nil || s.owner == nil || s.terminal || len(s.children) != 0 || s.phase < 1 {
				return nil, ErrUnsafeTeardown
			}
			s.terminal = true
			s.phase = 4
			latestKey = key
		}
	}
	s := states[latestKey]
	if s == nil || s.owner == nil {
		return nil, ErrLifecycleNotFound
	}
	if s.terminal {
		return nil, ErrUnsafeTeardown
	}
	owner := s.owner
	if owner.TabID == "" || owner.PaneID == "" || owner.LaunchID == "" || owner.Repository == "" || owner.TaskRef == "" || owner.Lane == "" || (!s.provisional && owner.SessionID == "") {
		return nil, ErrUnsafeTeardown
	}
	children := make([]Identity, 0, len(s.children))
	for _, c := range s.children {
		children = append(children, c)
	}
	pending := make(map[string]bool, len(s.intents))
	for key := range s.intents {
		pending[key] = true
	}
	l := &Lifecycle{Tree: tree, Sink: sink, Inventory: Inventory{Owner: *owner, Children: children}, RecoveredPhase: s.phase, PendingIntents: pending}
	return l, nil
}

func DiscoverLifecycle(tabID string, tree DescendantTree, sink ReceiptSink) (*Lifecycle, error) {
	root := os.Getenv("HERD_TOOLCHILD_RECEIPT_ROOT")
	if root == "" {
		cache, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(cache, "Herdforge", "toolchild")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var found *Lifecycle
	for _, path := range paths {
		candidateSink := sink
		if js, ok := sink.(*JSONLSink); ok && js.Path == "" {
			candidateSink = &JSONLSink{Path: path}
		}
		l, err := LoadLifecycle(path, tabID, tree, candidateSink)
		if err != nil {
			if errors.Is(err, ErrLifecycleNotFound) {
				continue
			}
			// Every file in the authenticated receipt root is controlled state.
			// A malformed or conflicting file cannot be hidden by another clean
			// candidate or by a formatting/escaping trick.
			return nil, err
		}
		if found != nil {
			return nil, ErrUnsafeTeardown
		}
		found = l
	}
	if found == nil {
		return nil, ErrUnsafeTeardown
	}
	return found, nil
}

func (l *Lifecycle) Reconcile(event string) error {
	if l == nil || l.Sink == nil {
		return ErrUnsafeTeardown
	}
	if !l.Bound() {
		return fmt.Errorf("provisional lifecycle is not bound; exact owner/process identity unavailable: %w", ErrUnsafeTeardown)
	}
	if l.RecoveredPhase < 3 {
		if err := l.Begin(); err != nil {
			return err
		}
	}
	if event != "done" && event != "failed-launch" && event != "recovery" && event != "tab-close" {
		return ErrUnsafeTeardown
	}
	children := append([]Identity(nil), l.Inventory.Children...)
	if l.RecoveredPhase >= 3 && len(l.PendingIntents) > 0 {
		pending := children[:0]
		for _, child := range children {
			if l.PendingIntents[childGenerationKey(child)] {
				pending = append(pending, child)
			}
		}
		children = pending
	}
	for _, child := range children {
		key := childGenerationKey(child)
		if !l.PendingIntents[key] {
			if err := l.Sink.Write(Receipt{Action: "reap-intent", Identity: child, Reason: event}); err != nil {
				return err
			}
		}
		r, err := Teardown(l.Tree, &l.Inventory, child.PID)
		if err != nil {
			return err
		}
		if err := l.Sink.Write(r); err != nil {
			_ = l.Inventory.Add(child) // retain exact authority for restart recovery
			return err
		}
		delete(l.PendingIntents, key)
	}
	l.RecoveredPhase = 0
	return nil
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
