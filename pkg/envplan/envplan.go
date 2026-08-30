// Package envplan owns the durable, least-privilege admission record for an
// environment action.  It deliberately records capabilities and evidence,
// never environment values or credentials.
package envplan

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

var (
	ErrNotFound  = errors.New("envplan: plan not found")
	ErrStale     = errors.New("envplan: plan is stale")
	ErrUnplanned = errors.New("envplan: external action is not planned")
	ErrDenied    = errors.New("envplan: operator grant is required")
)

// Capability names an external authority.  It is intentionally not a secret.
type Capability string

const (
	CapabilityNetwork    Capability = "network"
	CapabilityBoardWrite Capability = "board-write"
	CapabilityCredential Capability = "credential-broker"
)

// Binding is the FAC-235 runstate identity that invalidates the plan on any
// task, provider, graph, or durable-run revision drift.
type Binding struct {
	TaskRef          string `json:"task_ref"`
	TaskID           string `json:"task_id"`
	Provider         string `json:"provider"`
	ProviderRevision string `json:"provider_revision"`
	GraphRevision    string `json:"graph_revision"`
	RunID            string `json:"run_id"`
	RunRevision      int64  `json:"run_revision"`
	// RecoveryFromRevision is non-zero only for an explicit, exact-task
	// stale-run recovery. It makes recovery authority part of the immutable
	// plan binding instead of inferring it from an ordinary revision number.
	RecoveryFromRevision int64 `json:"recovery_from_revision,omitempty"`
}

func (b Binding) Recovered() bool {
	return b.RecoveryFromRevision > 0 && b.RunRevision == b.RecoveryFromRevision+1
}

type Evidence struct {
	Authority string `json:"authority"`
	Revision  string `json:"revision"`
	Subject   string `json:"subject"`
}

type Request struct {
	Capability Capability `json:"capability"`
	Evidence   Evidence   `json:"evidence"`
}

type Grant struct {
	Capability Capability `json:"capability"`
	Operator   string     `json:"operator"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

type Plan struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Binding       Binding   `json:"binding"`
	Requests      []Request `json:"requests"`
	Grants        []Grant   `json:"grants,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open environment plan: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS environment_plans (id TEXT PRIMARY KEY, body BLOB NOT NULL)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize environment plan: %w", err)
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create stores a canonical plan.  Repeating the same binding+requests is
// idempotent; a caller cannot silently replace a different durable plan.
func (s *Store) Create(ctx context.Context, p Plan) (*Plan, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("envplan: nil store")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.CreatedAt = p.CreatedAt.UTC()
	p.ExpiresAt = p.ExpiresAt.UTC()
	p.SchemaVersion = SchemaVersion
	canonicalize(&p)
	if p.ID == "" {
		p.ID = planID(p.Binding, p.Requests)
	}
	if err := validate(p); err != nil {
		return nil, err
	}
	if old, err := s.Load(ctx, p.ID); err == nil {
		if old.Binding != p.Binding || !sameRequests(old.Requests, p.Requests) {
			return nil, fmt.Errorf("%w: id collides with different plan", ErrStale)
		}
		return old, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO environment_plans(id, body) VALUES (?, ?)`, p.ID, b); err != nil {
		return nil, fmt.Errorf("store environment plan: %w", err)
	}
	return clone(p)
}
func (s *Store) Load(ctx context.Context, id string) (*Plan, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("envplan: nil store")
	}
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM environment_plans WHERE id=?`, id).Scan(&b); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%w: corrupt plan", ErrStale)
	}
	canonicalize(&p)
	if err := validate(p); err != nil {
		return nil, err
	}
	return clone(p)
}

// Grant records an operator decision without accepting any credential value.
// Repeating the same approval is idempotent; expiry can only move forward.
func (s *Store) Grant(ctx context.Context, id string, cap Capability, operator string, expiry time.Time) (*Plan, error) {
	p, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(operator) == "" || expiry.IsZero() || !expiry.After(time.Now().UTC()) {
		return nil, fmt.Errorf("%w: invalid operator or expiry", ErrDenied)
	}
	if !requested(p.Requests, cap) {
		return nil, fmt.Errorf("%w: %s", ErrUnplanned, cap)
	}
	for i := range p.Grants {
		if p.Grants[i].Capability == cap && p.Grants[i].Operator == operator {
			if p.Grants[i].ExpiresAt.Before(expiry.UTC()) {
				p.Grants[i].ExpiresAt = expiry.UTC()
			}
			return s.replace(ctx, *p)
		}
	}
	p.Grants = append(p.Grants, Grant{Capability: cap, Operator: operator, ExpiresAt: expiry.UTC()})
	canonicalize(p)
	return s.replace(ctx, *p)
}

// Revoke removes every active grant for one requested capability. It never
// changes the plan binding or requested capability set, so a later grant is a
// new attributable operator decision rather than a silent renewal.
func (s *Store) Revoke(ctx context.Context, id string, cap Capability) (*Plan, error) {
	p, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !requested(p.Requests, cap) {
		return nil, fmt.Errorf("%w: %s", ErrUnplanned, cap)
	}
	grants := p.Grants[:0]
	for _, grant := range p.Grants {
		if grant.Capability != cap {
			grants = append(grants, grant)
		}
	}
	p.Grants = grants
	return s.replace(ctx, *p)
}

// Authorize is the sole action gate.  The caller supplies freshly observed
// FAC-235 binding; any drift, expiry, unplanned action, or missing grant fails.
func (s *Store) Authorize(ctx context.Context, id string, live Binding, cap Capability, now time.Time) error {
	p, err := s.Load(ctx, id)
	if err != nil {
		return err
	}
	if p.Binding != live || !now.UTC().Before(p.ExpiresAt) {
		return ErrStale
	}
	if !requested(p.Requests, cap) {
		return fmt.Errorf("%w: %s", ErrUnplanned, cap)
	}
	for _, g := range p.Grants {
		if g.Capability == cap && now.UTC().Before(g.ExpiresAt) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrDenied, cap)
}

// NeedsApproval lets a caller avoid prompting an operator for an empty plan.
func (p Plan) NeedsApproval() bool { return len(p.Requests) != 0 }

func (s *Store) replace(ctx context.Context, p Plan) (*Plan, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE environment_plans SET body=? WHERE id=?`, b, p.ID)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, ErrNotFound
	}
	return clone(p)
}
func requested(rs []Request, cap Capability) bool {
	for _, r := range rs {
		if r.Capability == cap {
			return true
		}
	}
	return false
}
func canonicalize(p *Plan) {
	sort.Slice(p.Requests, func(i, j int) bool { return p.Requests[i].Capability < p.Requests[j].Capability })
	sort.Slice(p.Grants, func(i, j int) bool {
		if p.Grants[i].Capability == p.Grants[j].Capability {
			return p.Grants[i].Operator < p.Grants[j].Operator
		}
		return p.Grants[i].Capability < p.Grants[j].Capability
	})
}
func sameRequests(a, b []Request) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func clone(p Plan) (*Plan, error) {
	b, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	var q Plan
	e = json.Unmarshal(b, &q)
	return &q, e
}
func planID(b Binding, r []Request) string {
	x := struct {
		B Binding
		R []Request
	}{b, r}
	raw, _ := json.Marshal(x)
	sum := sha256.Sum256(raw)
	return "env-" + hex.EncodeToString(sum[:16])
}
func validate(p Plan) error {
	if p.SchemaVersion != SchemaVersion || p.ID == "" || p.Binding.TaskRef == "" || p.Binding.TaskID == "" || p.Binding.Provider == "" || p.Binding.ProviderRevision == "" || p.Binding.GraphRevision == "" || p.Binding.RunID == "" || p.Binding.RunRevision < 1 || p.Binding.RecoveryFromRevision < 0 || (p.Binding.RecoveryFromRevision > 0 && !p.Binding.Recovered()) || p.ExpiresAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) {
		return fmt.Errorf("%w: incomplete plan", ErrStale)
	}
	seen := map[Capability]bool{}
	for _, r := range p.Requests {
		if r.Capability == "" || seen[r.Capability] || (r.Evidence.Authority != "security" && r.Evidence.Authority != "harness") || r.Evidence.Revision == "" || r.Evidence.Subject == "" {
			return fmt.Errorf("%w: invalid capability request", ErrStale)
		}
		seen[r.Capability] = true
	}
	return nil
}
