package memory

// Scoped memory is deliberately a small local receipt store. It does not
// search remotely or synchronize agents: every admission decision is made
// against a registered run/task/role scope and written to append-only JSONL.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ScopeKind string

const (
	ScopeGlobal ScopeKind = "global"
	ScopeRun    ScopeKind = "run"
	ScopeTask   ScopeKind = "task"
	ScopeRole   ScopeKind = "role"
)

var (
	ErrUnauthorized    = errors.New("scoped memory: unauthorized")
	ErrCrossTask       = errors.New("scoped memory: cross-task access denied")
	ErrStaleRevision   = errors.New("scoped memory: stale revision")
	ErrExpired         = errors.New("scoped memory: proposal evidence expired")
	ErrUnknownScope    = errors.New("scoped memory: unknown scope")
	ErrInvalidScope    = errors.New("scoped memory: invalid scope")
	ErrUnknownProposal = errors.New("scoped memory: unknown proposal")
)

// Scope is the complete authority boundary. Revision is the task/provider/
// graph revision selected by the caller; no empty or drifting revision is
// admitted. Readers and Writers contain authenticated actor IDs.
type Scope struct {
	Kind     ScopeKind `json:"kind"`
	RunID    string    `json:"run_id,omitempty"`
	TaskID   string    `json:"task_id,omitempty"`
	Role     string    `json:"role,omitempty"`
	Revision string    `json:"revision"`
	Readers  []string  `json:"readers"`
	Writers  []string  `json:"writers"`
}

type Actor struct {
	ID            string `json:"id"`
	Role          string `json:"role"`
	Authenticated bool   `json:"authenticated"`
}

type Proposal struct {
	ID             string    `json:"id"`
	Scope          Scope     `json:"scope"`
	Actor          Actor     `json:"actor"`
	Content        string    `json:"content"`
	SourceEvidence string    `json:"source_evidence"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	RetainUntil    time.Time `json:"retain_until"`
}

type SharedKnowledge struct {
	ID             string    `json:"id"`
	ProposalID     string    `json:"proposal_id"`
	Scope          Scope     `json:"scope"`
	Content        string    `json:"content"`
	SourceEvidence string    `json:"source_evidence"`
	TaskEvidence   string    `json:"task_evidence"`
	PromotedBy     Actor     `json:"promoted_by"`
	PromotedAt     time.Time `json:"promoted_at"`
}

type Evidence struct {
	Action     string    `json:"action"`
	Actor      Actor     `json:"actor"`
	MemoryID   string    `json:"memory_id"`
	Scope      Scope     `json:"scope"`
	TaskID     string    `json:"task_id,omitempty"`
	Revision   string    `json:"revision"`
	OccurredAt time.Time `json:"occurred_at"`
}

type ScopeRelation struct {
	FromTask string `json:"from_task"`
	ToTask   string `json:"to_task"`
	Revision string `json:"revision"`
}

type ProposalRequest struct {
	Scope          Scope
	Actor          Actor
	Content        string
	SourceEvidence string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RetainUntil    time.Time
}

type PromotionRequest struct {
	ProposalID   string
	Actor        Actor
	Revision     string
	TaskEvidence string
	PromotedAt   time.Time
}

type ReadRequest struct {
	Actor    Actor
	RunID    string
	TaskID   string
	Role     string
	Revision string
	ReadAt   time.Time
}

// ScopedMemoryStore has no delete/update API. Its three JSONL files are the
// durable proposal, promotion, and read/promotion evidence receipts.
type ScopedMemoryStore struct {
	mu        sync.Mutex
	dir       string
	scopes    map[string]Scope
	relations map[string]ScopeRelation
	proposals map[string]Proposal
	promoted  map[string]SharedKnowledge
}

func NewScopedMemoryStore(dir string) (*ScopedMemoryStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("scoped memory: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("scoped memory: create directory: %w", err)
	}
	store := &ScopedMemoryStore{dir: filepath.Clean(dir), scopes: map[string]Scope{}, relations: map[string]ScopeRelation{}, proposals: map[string]Proposal{}, promoted: map[string]SharedKnowledge{}}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *ScopedMemoryStore) RegisterScope(scope Scope) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scope = canonicalScope(scope)
	if existing, ok := s.scopes[scopeKey(scope)]; ok && sameScope(existing, scope) {
		return nil
	}
	if err := s.append("scopes.jsonl", scope); err != nil {
		return err
	}
	s.scopes[scopeKey(scope)] = scope
	return nil
}

func (s *ScopedMemoryStore) AuthorizeRelation(relation ScopeRelation) error {
	if strings.TrimSpace(relation.FromTask) == "" || strings.TrimSpace(relation.ToTask) == "" || strings.TrimSpace(relation.Revision) == "" {
		return ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.relations[relationKey(relation)]; ok {
		return nil
	}
	if err := s.append("relations.jsonl", relation); err != nil {
		return err
	}
	s.relations[relationKey(relation)] = relation
	return nil
}

func (s *ScopedMemoryStore) Propose(req ProposalRequest) (Proposal, error) {
	if !req.Actor.Authenticated || !allowed(req.Scope.Writers, req.Actor.ID) {
		return Proposal{}, ErrUnauthorized
	}
	if req.Scope.Kind != ScopeTask {
		return Proposal{}, fmt.Errorf("%w: proposals must be task scoped", ErrInvalidScope)
	}
	if err := validateScope(req.Scope); err != nil {
		return Proposal{}, err
	}
	if strings.TrimSpace(req.Content) == "" || strings.TrimSpace(req.SourceEvidence) == "" || req.CreatedAt.IsZero() || req.ExpiresAt.IsZero() || req.RetainUntil.IsZero() || !req.ExpiresAt.After(req.CreatedAt) || req.RetainUntil.Before(req.ExpiresAt) {
		return Proposal{}, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	registered, ok := s.scopes[scopeKey(req.Scope)]
	if !ok || !sameScope(registered, req.Scope) {
		return Proposal{}, ErrUnknownScope
	}
	p := Proposal{Scope: registered, Actor: req.Actor, Content: req.Content, SourceEvidence: req.SourceEvidence, CreatedAt: req.CreatedAt.UTC(), ExpiresAt: req.ExpiresAt.UTC(), RetainUntil: req.RetainUntil.UTC()}
	p.ID = proposalID(p)
	if old, ok := s.proposals[p.ID]; ok {
		return old, nil
	}
	if err := s.append("proposals.jsonl", p); err != nil {
		return Proposal{}, err
	}
	s.proposals[p.ID] = p
	return p, nil
}

func (s *ScopedMemoryStore) Promote(req PromotionRequest) (SharedKnowledge, error) {
	if !req.Actor.Authenticated || (req.Actor.Role != "reviewer" && req.Actor.Role != "coordinator") {
		return SharedKnowledge{}, ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[req.ProposalID]
	if !ok {
		return SharedKnowledge{}, ErrUnknownProposal
	}
	if req.Revision != p.Scope.Revision {
		return SharedKnowledge{}, ErrStaleRevision
	}
	if req.PromotedAt.IsZero() || !req.PromotedAt.Before(p.ExpiresAt) {
		return SharedKnowledge{}, ErrExpired
	}
	if strings.TrimSpace(req.TaskEvidence) == "" {
		return SharedKnowledge{}, ErrInvalidScope
	}
	id := "promotion-" + p.ID
	if old, ok := s.promoted[id]; ok {
		return old, nil
	}
	global := Scope{Kind: ScopeGlobal, Revision: p.Scope.Revision, Readers: append([]string(nil), p.Scope.Readers...)}
	k := SharedKnowledge{ID: id, ProposalID: p.ID, Scope: global, Content: p.Content, SourceEvidence: p.SourceEvidence, TaskEvidence: req.TaskEvidence, PromotedBy: req.Actor, PromotedAt: req.PromotedAt.UTC()}
	if err := s.append("promotions.jsonl", k); err != nil {
		return SharedKnowledge{}, err
	}
	if err := s.recordEvidenceLocked("promotion", req.Actor, k.ID, p.Scope, p.Scope.TaskID, req.Revision, req.PromotedAt); err != nil {
		return SharedKnowledge{}, err
	}
	s.promoted[id] = k
	return k, nil
}

// Inject returns only knowledge authorized for this exact task/run/role and
// records each returned item. A stale registered scope rejects packet building
// rather than allowing silently stale context to reach an agent.
func (s *ScopedMemoryStore) Inject(req ReadRequest) ([]SharedKnowledge, error) {
	if !req.Actor.Authenticated {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(req.TaskID) == "" || strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.Role) == "" || strings.TrimSpace(req.Revision) == "" {
		return nil, ErrInvalidScope
	}
	if req.ReadAt.IsZero() {
		return nil, ErrInvalidScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range s.scopes {
		if scope.Revision != req.Revision && scopeMatchesRequest(scope, req) {
			return nil, ErrStaleRevision
		}
	}
	out := make([]SharedKnowledge, 0)
	for _, proposal := range s.proposals {
		if proposal.Scope.Revision != req.Revision || !allowed(proposal.Scope.Readers, req.Actor.ID) || !scopeAccessible(s.relations, proposal.Scope, req) {
			continue
		}
		out = append(out, SharedKnowledge{ID: proposal.ID, ProposalID: proposal.ID, Scope: proposal.Scope, Content: proposal.Content, SourceEvidence: proposal.SourceEvidence})
	}
	for _, knowledge := range s.promoted {
		if knowledge.Scope.Revision != req.Revision || !allowed(knowledge.Scope.Readers, req.Actor.ID) {
			continue
		}
		out = append(out, knowledge)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for _, knowledge := range out {
		if err := s.recordEvidenceLocked("read", req.Actor, knowledge.ID, knowledge.Scope, req.TaskID, req.Revision, req.ReadAt); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ScopedMemoryStore) Evidence() ([]Evidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, "evidence.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]Evidence, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Evidence
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("scoped memory: decode evidence: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

// load rebuilds the in-memory authority and idempotency indexes from the
// append-only receipts. A malformed receipt is a hard error: silently
// discarding it would turn durable authorization into an unaudited bypass.
func (s *ScopedMemoryStore) load() error {
	if err := readJSONL(filepath.Join(s.dir, "scopes.jsonl"), func(data []byte) error {
		var scope Scope
		if err := json.Unmarshal(data, &scope); err != nil {
			return err
		}
		if err := validateScope(scope); err != nil {
			return err
		}
		s.scopes[scopeKey(scope)] = canonicalScope(scope)
		return nil
	}); err != nil {
		return fmt.Errorf("scoped memory: load scopes: %w", err)
	}
	if err := readJSONL(filepath.Join(s.dir, "relations.jsonl"), func(data []byte) error {
		var relation ScopeRelation
		if err := json.Unmarshal(data, &relation); err != nil {
			return err
		}
		if strings.TrimSpace(relation.FromTask) == "" || strings.TrimSpace(relation.ToTask) == "" || strings.TrimSpace(relation.Revision) == "" {
			return ErrInvalidScope
		}
		s.relations[relationKey(relation)] = relation
		return nil
	}); err != nil {
		return fmt.Errorf("scoped memory: load relations: %w", err)
	}
	if err := readJSONL(filepath.Join(s.dir, "proposals.jsonl"), func(data []byte) error {
		var proposal Proposal
		if err := json.Unmarshal(data, &proposal); err != nil {
			return err
		}
		if proposal.ID == "" || proposal.ID != proposalID(proposal) {
			return ErrInvalidScope
		}
		s.proposals[proposal.ID] = proposal
		return nil
	}); err != nil {
		return fmt.Errorf("scoped memory: load proposals: %w", err)
	}
	if err := readJSONL(filepath.Join(s.dir, "promotions.jsonl"), func(data []byte) error {
		var knowledge SharedKnowledge
		if err := json.Unmarshal(data, &knowledge); err != nil {
			return err
		}
		if knowledge.ID == "" || knowledge.ID != "promotion-"+knowledge.ProposalID {
			return ErrInvalidScope
		}
		s.promoted[knowledge.ID] = knowledge
		return nil
	}); err != nil {
		return fmt.Errorf("scoped memory: load promotions: %w", err)
	}
	return nil
}

func readJSONL(path string, consume func([]byte) error) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := consume([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func (s *ScopedMemoryStore) recordEvidenceLocked(action string, actor Actor, id string, scope Scope, task, revision string, at time.Time) error {
	return s.append("evidence.jsonl", Evidence{Action: action, Actor: actor, MemoryID: id, Scope: scope, TaskID: task, Revision: revision, OccurredAt: at.UTC()})
}
func (s *ScopedMemoryStore) append(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scoped memory: open %s: %w", name, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("scoped memory: append %s: %w", name, err)
	}
	return f.Sync()
}

func validateScope(scope Scope) error {
	if scope.Kind != ScopeGlobal && scope.Kind != ScopeRun && scope.Kind != ScopeTask && scope.Kind != ScopeRole {
		return ErrInvalidScope
	}
	if strings.TrimSpace(scope.Revision) == "" || len(scope.Readers) == 0 {
		return ErrInvalidScope
	}
	if scope.Kind == ScopeTask && strings.TrimSpace(scope.TaskID) == "" {
		return ErrInvalidScope
	}
	if scope.Kind == ScopeRun && strings.TrimSpace(scope.RunID) == "" {
		return ErrInvalidScope
	}
	if scope.Kind == ScopeRole && (strings.TrimSpace(scope.TaskID) == "" || strings.TrimSpace(scope.Role) == "") {
		return ErrInvalidScope
	}
	return nil
}
func canonicalScope(s Scope) Scope {
	s.Readers = append([]string(nil), s.Readers...)
	s.Writers = append([]string(nil), s.Writers...)
	sort.Strings(s.Readers)
	sort.Strings(s.Writers)
	return s
}
func sameScope(a, b Scope) bool {
	a, b = canonicalScope(a), canonicalScope(b)
	return a.Kind == b.Kind && a.RunID == b.RunID && a.TaskID == b.TaskID && a.Role == b.Role && a.Revision == b.Revision && strings.Join(a.Readers, "\x00") == strings.Join(b.Readers, "\x00") && strings.Join(a.Writers, "\x00") == strings.Join(b.Writers, "\x00")
}
func allowed(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
func scopeKey(s Scope) string {
	return string(s.Kind) + "\x00" + s.RunID + "\x00" + s.TaskID + "\x00" + s.Role
}
func relationKey(r ScopeRelation) string { return r.FromTask + "\x00" + r.ToTask + "\x00" + r.Revision }
func scopeMatchesRequest(s Scope, r ReadRequest) bool {
	switch s.Kind {
	case ScopeGlobal:
		return true
	case ScopeRun:
		return s.RunID == r.RunID
	case ScopeTask:
		return s.TaskID == r.TaskID
	case ScopeRole:
		return s.TaskID == r.TaskID && s.Role == r.Role
	}
	return false
}
func scopeAccessible(relations map[string]ScopeRelation, scope Scope, request ReadRequest) bool {
	if scopeMatchesRequest(scope, request) {
		return true
	}
	if scope.Kind != ScopeTask || scope.Revision != request.Revision {
		return false
	}
	_, ok := relations[relationKey(ScopeRelation{FromTask: request.TaskID, ToTask: scope.TaskID, Revision: request.Revision})]
	return ok
}
func proposalID(p Proposal) string {
	b, _ := json.Marshal(struct {
		Scope    Scope     `json:"scope"`
		Actor    string    `json:"actor"`
		Content  string    `json:"content"`
		Evidence string    `json:"evidence"`
		Created  time.Time `json:"created"`
	}{p.Scope, p.Actor.ID, p.Content, p.SourceEvidence, p.CreatedAt})
	sum := sha256.Sum256(b)
	return "proposal-" + hex.EncodeToString(sum[:])
}
