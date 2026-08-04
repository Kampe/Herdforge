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

const (
	ReasonMissingScope        = "missing_scope"
	ReasonAmbiguousIdentity   = "ambiguous_identity"
	ReasonInvalidGeneration   = "invalid_generation"
	ReasonGraphInvalid        = "graph_invalid"
	ReasonOwnershipUnreadable = "ownership_unreadable"
	ReasonScopeOverlap        = "scope_overlap"
	ReasonIdentityConflict    = "identity_conflict"
	ReasonCASContention       = "cas_contention"
	ReasonContextCanceled     = "context_canceled"
	ReasonInvalidScope        = "invalid_scope"
	ReasonGraphUntrusted      = "graph_untrusted"
	ReasonInvalidState        = "invalid_state"
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
	Generation    int64  `json:"generation"`
	Scope         Scope  `json:"scope"`
	State         State  `json:"state"`
	GraphRevision string `json:"graph_revision"`
	GraphFiles    int    `json:"graph_files"`
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
	ExpectedGraphFiles    int
}

type Evidence struct {
	Repository            string   `json:"repository"`
	Task                  string   `json:"task"`
	Branch                string   `json:"branch"`
	Generation            int64    `json:"generation"`
	ConflictRepository    string   `json:"conflict_repository,omitempty"`
	ConflictTask          string   `json:"conflict_task,omitempty"`
	ConflictBranch        string   `json:"conflict_branch,omitempty"`
	ConflictGeneration    int64    `json:"conflict_generation,omitempty"`
	ConflictGraphRevision string   `json:"conflict_graph_revision,omitempty"`
	ConflictGraphFiles    int      `json:"conflict_graph_files,omitempty"`
	Packages              []string `json:"packages,omitempty"`
	Files                 []string `json:"files,omitempty"`
	Symbols               []string `json:"symbols,omitempty"`
	GraphRevision         string   `json:"graph_revision"`
	GraphFiles            int      `json:"graph_files"`
	Reason                string   `json:"reason"`
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

// AtomicReleaseStore removes an ownership and records its proof in one
// durable transaction. Fence uses this stronger contract when available.
type AtomicReleaseStore interface {
	Release(context.Context, ReleaseRequest) error
}

// ReleaseProofStore durably records a successful fenced release. Implementations
// must key the record by the exact ownership identity, generation, scope, and
// graph binding so a stale release cannot overwrite a newer proof.
type ReleaseProofStore interface {
	RecordReleaseProof(context.Context, ReleaseRequest) error
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
	Graph  GraphAuthority
}

// TrustedGraph carries observed graph data and expectations from the
// separately wired authority. The request's graph fields are never sufficient
// to authorize an acquire.
type TrustedGraph struct {
	Snapshot         Graph
	ExpectedRevision string
	ExpectedFiles    int
}

// GraphAuthority is a separately wired source of trusted graph truth and its
// expected revision/file-count contract.
type GraphAuthority interface {
	Current(context.Context) (TrustedGraph, error)
}

// ScopeAuthority resolves task scope from a separately trusted, revision-bound
// source. Acquire callers must not be treated as scope authority.
type ScopeAuthority interface {
	Resolve(context.Context, string, string, string) (Scope, error)
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)

func (s Scope) validate() error {
	if len(s.Packages)+len(s.Files)+len(s.Symbols) == 0 {
		return errors.New("missing scope")
	}
	for _, group := range [][]string{s.Packages, s.Files, s.Symbols} {
		for _, value := range group {
			if !rawScopeValueValid(value) {
				return fmt.Errorf("invalid scope value")
			}
		}
	}
	return nil
}

func rawScopeValueValid(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || path.IsAbs(value) || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "~") || (len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z'))) || !tokenPattern.MatchString(value) {
		return false
	}
	if strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return true
}

func canonicalScope(s Scope) (Scope, error) {
	clean := func(values []string, symbols bool) ([]string, error) {
		seen := make(map[string]bool, len(values))
		out := make([]string, 0, len(values))
		for _, value := range values {
			if !rawScopeValueValid(value) {
				return nil, errors.New("invalid scope value")
			}
			if symbols {
				parts := strings.Split(value, "::")
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], ":") {
					return nil, errors.New("invalid symbol declaration")
				}
			} else if strings.Contains(value, "::") {
				return nil, errors.New("invalid declaration")
			}
			value = strings.ReplaceAll(value, "\\", "/")
			if symbols {
				if i := strings.Index(value, "::"); i > 0 {
					value = path.Clean(value[:i]) + "::" + value[i+2:]
				}
			} else {
				value = path.Clean(value)
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	var err error
	if s.Packages, err = clean(s.Packages, false); err != nil {
		return Scope{}, err
	}
	if s.Files, err = clean(s.Files, false); err != nil {
		return Scope{}, err
	}
	if s.Symbols, err = clean(s.Symbols, true); err != nil {
		return Scope{}, err
	}
	return s, s.validate()
}

func (i Identity) validate() error {
	if !tokenPattern.MatchString(i.Repository) || !tokenPattern.MatchString(i.Branch) || !tokenPattern.MatchString(i.Task) {
		return errors.New("ambiguous identity")
	}
	return nil
}

func (g Graph) validate(expected string, expectedFiles int) error {
	if !g.Complete || expected == "" || expectedFiles <= 0 || g.Revision == "" || g.Revision != expected || g.Files != expectedFiles || g.Nodes <= 0 || g.Edges <= 0 || g.Files <= 0 || g.Flows <= 0 || g.Edges < g.Nodes {
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
	if !validState(o.State) {
		return errors.New("invalid state")
	}
	if _, err := canonicalScope(o.Scope); err != nil {
		return err
	}
	if o.GraphRevision == "" || o.GraphFiles <= 0 {
		return errors.New("unbound graph")
	}
	return nil
}

func validState(state State) bool {
	switch state {
	case Active, Clean, Done, Idle, Audit, Review:
		return true
	default:
		return false
	}
}

func overlap(a, b Scope) (Evidence, bool) {
	contains := func(container, item string) bool { return container == item || strings.HasPrefix(item, container+"/") }
	pairs := func(x, y []string, relation func(string, string) bool) []string {
		var out []string
		for _, left := range x {
			for _, right := range y {
				if relation(left, right) {
					out = append(out, left+"<->"+right)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	unique := func(in []string) []string {
		seen := map[string]bool{}
		out := make([]string, 0, len(in))
		for _, value := range in {
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		return out
	}
	packagePairs := unique(pairs(a.Packages, b.Packages, func(x, y string) bool { return contains(x, y) || contains(y, x) }))
	filePairs := pairs(a.Files, b.Files, func(x, y string) bool { return contains(x, y) || contains(y, x) })
	filePairs = append(filePairs, pairs(a.Packages, b.Files, contains)...)
	filePairs = append(filePairs, pairs(b.Packages, a.Files, func(x, y string) bool { return contains(x, y) })...)
	filePairs = unique(filePairs)
	symbolPairs := pairs(a.Symbols, b.Files, func(x, y string) bool { return strings.HasPrefix(x, y+"::") })
	symbolPairs = append(symbolPairs, pairs(b.Symbols, a.Files, func(x, y string) bool { return strings.HasPrefix(x, y+"::") })...)
	symbolPairs = append(symbolPairs, pairs(a.Packages, b.Symbols, func(x, y string) bool { return strings.HasPrefix(y, x+"/") })...)
	symbolPairs = unique(append(symbolPairs, pairs(b.Packages, a.Symbols, func(x, y string) bool { return strings.HasPrefix(y, x+"/") })...))
	symbolFile := func(value string) string {
		if i := strings.Index(value, "::"); i > 0 {
			return value[:i]
		}
		return ""
	}
	symbolPairs = unique(append(symbolPairs, pairs(a.Symbols, b.Symbols, func(x, y string) bool { return x == y || (symbolFile(x) != "" && symbolFile(x) == symbolFile(y)) })...))
	e := Evidence{Packages: packagePairs, Files: filePairs, Symbols: symbolPairs}
	return e, len(e.Packages)+len(e.Files)+len(e.Symbols) > 0
}

func bounded(in []string) []string {
	if len(in) > 8 {
		in = in[:8]
	}
	return in
}

func scopesEqual(a, b Scope) bool {
	return strings.Join(a.Packages, "\x00") == strings.Join(b.Packages, "\x00") && strings.Join(a.Files, "\x00") == strings.Join(b.Files, "\x00") && strings.Join(a.Symbols, "\x00") == strings.Join(b.Symbols, "\x00")
}

func cloneScope(s Scope) Scope {
	return Scope{Packages: append([]string(nil), s.Packages...), Files: append([]string(nil), s.Files...), Symbols: append([]string(nil), s.Symbols...)}
}

func cloneOwnership(o Ownership) Ownership { o.Scope = cloneScope(o.Scope); return o }
func cloneOwnerships(in []Ownership) []Ownership {
	out := make([]Ownership, len(in))
	for i, o := range in {
		out[i] = cloneOwnership(o)
	}
	return out
}

func (f Fence) Acquire(ctx context.Context, req AcquireRequest) (Decision, error) {
	if f.Store == nil {
		return Decision{}, errors.New("nil store")
	}
	canonical, err := canonicalScope(req.Scope)
	if err != nil {
		reason := ReasonInvalidScope
		if len(req.Scope.Packages)+len(req.Scope.Files)+len(req.Scope.Symbols) == 0 {
			reason = ReasonMissingScope
		}
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: reason}}, nil
	}
	req.Scope = canonical
	if err := req.Identity.validate(); err != nil {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: ReasonAmbiguousIdentity}}, nil
	}
	if req.Generation <= 0 {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: ReasonInvalidGeneration}}, nil
	}
	if !validState(req.State) {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: ReasonInvalidState}}, nil
	}
	if err := ctx.Err(); err != nil {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: ReasonContextCanceled}}, err
	}
	if f.Graph == nil {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, Reason: ReasonGraphUntrusted}}, nil
	}
	trusted, err := f.Graph.Current(ctx)
	if err != nil {
		return Decision{}, err
	}
	graph := trusted.Snapshot
	if err := graph.validate(trusted.ExpectedRevision, trusted.ExpectedFiles); err != nil {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, GraphRevision: graph.Revision, GraphFiles: graph.Files, Reason: ReasonGraphInvalid}}, nil
	}
	if req.ExpectedGraphRevision != "" && req.ExpectedGraphRevision != graph.Revision {
		return Decision{Evidence: Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, GraphRevision: graph.Revision, GraphFiles: graph.Files, Reason: ReasonGraphInvalid}}, nil
	}
	req.GraphRevision, req.GraphFiles = graph.Revision, graph.Files
	baseEvidence := Evidence{Repository: req.Repository, Task: req.Task, Branch: req.Branch, Generation: req.Generation, GraphRevision: graph.Revision, GraphFiles: graph.Files}
	for attempts := 0; attempts < 4; attempts++ {
		if err := ctx.Err(); err != nil {
			baseEvidence.Reason = ReasonContextCanceled
			return Decision{Evidence: baseEvidence}, err
		}
		s, err := f.Store.Read(ctx)
		if err != nil {
			return Decision{}, err
		}
		for _, owner := range s.Owners {
			if err := owner.validate(); err != nil {
				baseEvidence.Reason = ReasonOwnershipUnreadable
				return Decision{Evidence: baseEvidence}, nil
			}
			ownerScope, _ := canonicalScope(owner.Scope)
			owner.Scope = ownerScope
			if owner.Identity == req.Identity {
				if owner.Generation == req.Generation && owner.State == req.State && owner.GraphRevision == req.GraphRevision && owner.GraphFiles == req.GraphFiles && scopesEqual(owner.Scope, req.Scope) {
					copy := cloneOwnership(owner)
					return Decision{Granted: true, Lease: &copy, Evidence: baseEvidence}, nil
				}
				baseEvidence.Reason = ReasonIdentityConflict
				baseEvidence.ConflictRepository, baseEvidence.ConflictTask, baseEvidence.ConflictBranch, baseEvidence.ConflictGeneration = owner.Repository, owner.Task, owner.Branch, owner.Generation
				baseEvidence.ConflictGraphRevision, baseEvidence.ConflictGraphFiles = owner.GraphRevision, owner.GraphFiles
				return Decision{Evidence: baseEvidence}, nil
			}
			e, hit := overlap(req.Scope, owner.Scope)
			if hit {
				e.Repository, e.Task, e.Branch, e.Generation = req.Repository, req.Task, req.Branch, req.Generation
				e.ConflictRepository, e.ConflictTask, e.ConflictBranch, e.ConflictGeneration = owner.Repository, owner.Task, owner.Branch, owner.Generation
				e.ConflictGraphRevision, e.ConflictGraphFiles = owner.GraphRevision, owner.GraphFiles
				e.GraphRevision, e.GraphFiles, e.Reason = graph.Revision, graph.Files, ReasonScopeOverlap
				e.Packages, e.Files, e.Symbols = bounded(e.Packages), bounded(e.Files), bounded(e.Symbols)
				return Decision{Evidence: e}, nil
			}
		}
		if err := ctx.Err(); err != nil {
			baseEvidence.Reason = ReasonContextCanceled
			return Decision{Evidence: baseEvidence}, err
		}
		next := append(cloneOwnerships(s.Owners), req.Ownership)
		won, err := f.Store.CompareAndSwap(ctx, s.Revision, next)
		if err != nil {
			return Decision{}, err
		}
		if won {
			lease := cloneOwnership(req.Ownership)
			return Decision{Granted: true, Lease: &lease, Evidence: baseEvidence}, nil
		}
	}
	baseEvidence.Reason = ReasonCASContention
	return Decision{Evidence: baseEvidence}, nil
}

func (f Fence) Release(ctx context.Context, req ReleaseRequest) error {
	if f.Store == nil || f.Verify == nil {
		return ErrBlocked
	}
	canonical, err := canonicalScope(req.Scope)
	if err != nil || req.Generation <= 0 || !validState(req.State) || !tokenPattern.MatchString(req.Repository) || !tokenPattern.MatchString(req.Branch) || !tokenPattern.MatchString(req.Task) {
		return ErrBlocked
	}
	req.Scope = canonical
	if req.Authority != RootAdmittedMerge && req.Authority != FencedAbandonment && req.Authority != CompensatedNoCandidate {
		return ErrBlocked
	}
	if !f.Verify(ctx, req) {
		return ErrBlocked
	}
	if atomicStore, ok := f.Store.(AtomicReleaseStore); ok {
		return atomicStore.Release(ctx, req)
	}
	for attempts := 0; attempts < 4; attempts++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, err := f.Store.Read(ctx)
		if err != nil {
			return err
		}
		found := -1
		for i, owner := range s.Owners {
			ownerScope, _ := canonicalScope(owner.Scope)
			owner.Scope = ownerScope
			if owner.Identity == req.Identity && owner.Generation == req.Generation && owner.State == req.State && owner.GraphRevision == req.GraphRevision && owner.GraphFiles == req.GraphFiles && scopesEqual(owner.Scope, req.Scope) {
				found = i
				break
			}
		}
		if found < 0 {
			return ErrBlocked
		}
		next := cloneOwnerships(s.Owners[:found])
		next = append(next, cloneOwnerships(s.Owners[found+1:])...)
		if err := ctx.Err(); err != nil {
			return err
		}
		won, err := f.Store.CompareAndSwap(ctx, s.Revision, next)
		if err != nil {
			return err
		}
		if won {
			if proofs, ok := f.Store.(ReleaseProofStore); ok {
				if err := proofs.RecordReleaseProof(ctx, req); err != nil {
					return err
				}
			}
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
	return &MemoryStore{revision: 1, owners: cloneOwnerships(owners)}
}
func (m *MemoryStore) Read(context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{Revision: fmt.Sprint(m.revision), Owners: cloneOwnerships(m.owners)}, nil
}
func (m *MemoryStore) CompareAndSwap(_ context.Context, rev string, next []Ownership) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rev != fmt.Sprint(m.revision) {
		return false, nil
	}
	m.owners = cloneOwnerships(next)
	m.revision++
	return true, nil
}
