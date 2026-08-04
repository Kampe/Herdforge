// Package scopefence is the fail-closed authority for dispatch scope fences.
// It deliberately has no knowledge of boards, git, worktrees, or processes.
package scopefence

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	ErrBlocked = errors.New("scopefence: dispatch blocked")
	ErrCASLost = errors.New("scopefence: compare-and-swap lost")
)

type State string

const (
	Active State = "active"
	Clean  State = "clean"
	Done   State = "done"
	Idle   State = "idle"
	Audit  State = "audit"
	Review State = "review"
)

// Scope contains repository-relative lexical declarations. An empty scope is
// invalid: uncertainty must never become permission to dispatch.
type Scope struct {
	Packages []string `json:"packages"`
	Files    []string `json:"files"`
	Symbols  []string `json:"symbols"`
}

type Identity struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Task       string `json:"task"`
}

type Ownership struct {
	Identity
	Generation int64 `json:"generation"`
	Scope      Scope `json:"scope"`
	State      State `json:"state"`
}

type Graph struct {
	Revision string `json:"revision"`
	Nodes    int    `json:"nodes"`
	Edges    int    `json:"edges"`
	Files    int    `json:"files"`
	Flows    int    `json:"flows"`
	Complete bool   `json:"complete"`
}

type AcquireRequest struct {
	Ownership
	Graph                 Graph
	ExpectedGraphRevision string
}

type Evidence struct {
	Task       string   `json:"task"`
	Branch     string   `json:"branch"`
	Generation int64    `json:"generation"`
	Packages   []string `json:"packages,omitempty"`
	Files      []string `json:"files,omitempty"`
	Symbols    []string `json:"symbols,omitempty"`
	Reason     string   `json:"reason"`
}

type Decision struct {
	Granted  bool       `json:"granted"`
	Lease    *Ownership `json:"lease,omitempty"`
	Evidence Evidence   `json:"evidence"`
}

type Snapshot struct {
	Revision string
	Owners   []Ownership
}

// Store is the only persistence seam. CompareAndSwap must be atomic in the
// backing store; implementations must not emulate it with a read followed by
// an unconditional write.
type Store interface {
	Read(context.Context) (Snapshot, error)
	CompareAndSwap(context.Context, string, []Ownership) (bool, error)
}

type Authority int

const (
	RootAdmittedMerge Authority = iota + 1
	FencedAbandonment
	CompensatedNoCandidate
)

type ReleaseRequest struct {
	Ownership
	Authority Authority
	Proof     string
}

// ProofVerifier authenticates root admission or compensation. Fenced
// abandonment additionally requires an exact identity and generation match.
type ProofVerifier func(context.Context, ReleaseRequest) bool

type Fence struct {
	Store  Store
	Verify ProofVerifier
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)

func (s Scope) validate() error {
	if len(s.Packages)+len(s.Files)+len(s.Symbols) == 0 {
		return errors.New("missing scope")
	}
	for _, group := range [][]string{s.Packages, s.Files, s.Symbols} {
		for _, value := range group {
			if value == "" || strings.HasPrefix(value, "/") || path.IsAbs(value) || path.Clean(value) != value || strings.Contains(value, "..") || !tokenPattern.MatchString(value) {
				return fmt.Errorf("invalid scope value")
			}
		}
	}
	return nil
}

func (i Identity) validate() error {
	if !tokenPattern.MatchString(i.Repository) || !tokenPattern.MatchString(i.Branch) || !tokenPattern.MatchString(i.Task) {
		return errors.New("ambiguous identity")
	}
	return nil
}

func (g Graph) validate(expected string) error {
	if !g.Complete || g.Revision == "" || (expected != "" && g.Revision != expected) || g.Nodes <= 0 || g.Edges <= 0 || g.Files <= 0 || g.Flows <= 0 || g.Edges < g.Nodes {
		return errors.New("incomplete or implausible graph")
	}
	return nil
}

func (o Ownership) validate() error {
	if err := o.Identity.validate(); err != nil {
		return err
	}
	if o.Generation <= 0 {
		return errors.New("invalid generation")
	}
	return o.Scope.validate()
}

func overlap(a, b Scope) (Evidence, bool) {
	intersect := func(x, y []string) []string {
		m := map[string]bool{}
		for _, v := range x {
			m[v] = true
		}
		var z []string
		for _, v := range y {
			if m[v] {
				z = append(z, v)
			}
		}
		sort.Strings(z)
		return z
	}
	e := Evidence{Packages: intersect(a.Packages, b.Packages), Files: intersect(a.Files, b.Files), Symbols: intersect(a.Symbols, b.Symbols)}
	return e, len(e.Packages)+len(e.Files)+len(e.Symbols) > 0
}

func bounded(in []string) []string {
	if len(in) > 8 {
		in = in[:8]
	}
	return in
}

func (f Fence) Acquire(ctx context.Context, req AcquireRequest) (Decision, error) {
	if f.Store == nil {
		return Decision{}, errors.New("nil store")
	}
	if err := req.Ownership.validate(); err != nil {
		return Decision{Evidence: Evidence{Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: err.Error()}}, nil
	}
	if err := req.Graph.validate(req.ExpectedGraphRevision); err != nil {
		return Decision{Evidence: Evidence{Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: err.Error()}}, nil
	}
	for attempts := 0; attempts < 4; attempts++ {
		s, err := f.Store.Read(ctx)
		if err != nil {
			return Decision{}, err
		}
		for _, owner := range s.Owners {
			if owner.Identity == req.Identity {
				continue
			}
			if err := owner.validate(); err != nil {
				return Decision{Evidence: Evidence{Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: "unreadable active ownership"}}, nil
			}
			e, hit := overlap(req.Scope, owner.Scope)
			if hit {
				e.Task, e.Branch, e.Generation, e.Reason = req.Task, req.Branch, req.Generation, "scope overlap"
				e.Packages, e.Files, e.Symbols = bounded(e.Packages), bounded(e.Files), bounded(e.Symbols)
				return Decision{Evidence: e}, nil
			}
		}
		next := append(append([]Ownership(nil), s.Owners...), req.Ownership)
		won, err := f.Store.CompareAndSwap(ctx, s.Revision, next)
		if err != nil {
			return Decision{}, err
		}
		if won {
			return Decision{Granted: true, Lease: &req.Ownership, Evidence: Evidence{Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: "acquired"}}, nil
		}
	}
	return Decision{Evidence: Evidence{Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: "cas contention"}}, nil
}

func (f Fence) Release(ctx context.Context, req ReleaseRequest) error {
	if f.Store == nil || f.Verify == nil {
		return ErrBlocked
	}
	if err := req.Ownership.validate(); err != nil {
		return ErrBlocked
	}
	if req.Authority != RootAdmittedMerge && req.Authority != FencedAbandonment && req.Authority != CompensatedNoCandidate {
		return ErrBlocked
	}
	if !f.Verify(ctx, req) {
		return ErrBlocked
	}
	for attempts := 0; attempts < 4; attempts++ {
		s, err := f.Store.Read(ctx)
		if err != nil {
			return err
		}
		found := -1
		for i, owner := range s.Owners {
			if owner.Identity == req.Identity && owner.Generation == req.Generation {
				found = i
				break
			}
		}
		if found < 0 {
			return ErrBlocked
		}
		next := append([]Ownership(nil), s.Owners[:found]...)
		next = append(next, s.Owners[found+1:]...)
		won, err := f.Store.CompareAndSwap(ctx, s.Revision, next)
		if err != nil {
			return err
		}
		if won {
			return nil
		}
	}
	return ErrCASLost
}

type Candidate struct {
	Task         string
	Priority     int
	TicketNumber int
	Excluded     bool
}

func SortCandidates(c []Candidate) []Candidate {
	out := make([]Candidate, 0, len(c))
	for _, v := range c {
		if !v.Excluded {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].TicketNumber != out[j].TicketNumber {
			return out[i].TicketNumber < out[j].TicketNumber
		}
		return out[i].Task < out[j].Task
	})
	return out
}

type MemoryStore struct {
	mu       sync.Mutex
	revision int64
	owners   []Ownership
}

func NewMemoryStore(owners ...Ownership) *MemoryStore {
	return &MemoryStore{revision: 1, owners: append([]Ownership(nil), owners...)}
}
func (m *MemoryStore) Read(context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{Revision: fmt.Sprint(m.revision), Owners: append([]Ownership(nil), m.owners...)}, nil
}
func (m *MemoryStore) CompareAndSwap(_ context.Context, rev string, next []Ownership) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rev != fmt.Sprint(m.revision) {
		return false, nil
	}
	m.owners = append([]Ownership(nil), next...)
	m.revision++
	return true, nil
}
