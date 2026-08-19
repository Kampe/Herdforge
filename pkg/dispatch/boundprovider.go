package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// BindingAuthority is the canonical identity a receipt must match before it
// may drive any provider call: the CURRENT repository's configuration, not
// whatever repo or workspace happens to be focused. Empty authority fields
// (beyond the required trio) skip their comparison.
type BindingAuthority struct {
	Repository        string
	ProviderType      string
	ProjectID         string
	ProviderWorkspace string
	ProviderProfile   string
}

// AuthorityFromConfig derives the canonical binding authority from the
// repository's validated config, using the STABLE repository identity at
// root (normalized remote + configured name) rather than the configured
// name alone — two same-named repositories are distinct authorities
// (FAC-145). Falls back to the configured name only when identity cannot
// be derived, which Validate then treats as a normal (weaker) identity.
func AuthorityFromConfigAt(cfg *config.Config, repoRoot string) BindingAuthority {
	repo := cfg.Project.Name
	if id, err := RepositoryIdentity(repoRoot, cfg.Project.Name); err == nil {
		repo = id
	}
	return BindingAuthority{
		Repository:        repo,
		ProviderType:      cfg.TaskProvider.Type,
		ProjectID:         cfg.TaskProvider.ProjectID,
		ProviderWorkspace: cfg.TaskProvider.WorkspaceID,
		ProviderProfile:   cfg.TaskProvider.APIKeyEnv,
	}
}

// RepositoryIdentityOrName is the receipt-side counterpart: issuers stamp
// the same stable identity into TaskContext.Repository.
func RepositoryIdentityOrName(repoRoot, configuredName string) string {
	if id, err := RepositoryIdentity(repoRoot, configuredName); err == nil {
		return id
	}
	return configuredName
}

// AuthorityFromConfig is the legacy name-only form, retained for callers
// without a resolved root.
func AuthorityFromConfig(cfg *config.Config) BindingAuthority {
	return BindingAuthority{
		Repository:        cfg.Project.Name,
		ProviderType:      cfg.TaskProvider.Type,
		ProjectID:         cfg.TaskProvider.ProjectID,
		ProviderWorkspace: cfg.TaskProvider.WorkspaceID,
		ProviderProfile:   cfg.TaskProvider.APIKeyEnv,
	}
}

// matches rejects any receipt raised for a different repository, provider
// type, project, workspace, or credential profile than the canonical one.
func (a BindingAuthority) matches(tc TaskContext) error {
	if strings.TrimSpace(a.Repository) == "" || strings.TrimSpace(a.ProviderType) == "" || strings.TrimSpace(a.ProjectID) == "" {
		return fmt.Errorf("binding authority is incomplete (repository/provider_type/project_id required) — refusing all provider traffic (FAC-145)")
	}
	if err := tc.ForRepository(a.Repository); err != nil {
		return err
	}
	if tc.ProviderType != a.ProviderType {
		return fmt.Errorf("receipt for %s is bound to provider %q, not the configured %q (FAC-145: provider mismatch rejected)", tc.TaskRef, tc.ProviderType, a.ProviderType)
	}
	if tc.ProjectID != a.ProjectID {
		return fmt.Errorf("receipt for %s is bound to project %q, not the configured %q (FAC-145)", tc.TaskRef, tc.ProjectID, a.ProjectID)
	}
	if a.ProviderWorkspace != "" && tc.ProviderWorkspace != a.ProviderWorkspace {
		return fmt.Errorf("receipt for %s is bound to workspace %q, not the configured %q (FAC-145)", tc.TaskRef, tc.ProviderWorkspace, a.ProviderWorkspace)
	}
	if a.ProviderProfile != "" && tc.ProviderProfile != a.ProviderProfile {
		return fmt.Errorf("receipt for %s references credential profile %q, not the configured %q (FAC-145)", tc.TaskRef, tc.ProviderProfile, a.ProviderProfile)
	}
	return nil
}

// ContextBoundProvider is the enforcement layer between a launch receipt and
// a task provider (FAC-145): every read and mutation is authorized against
// the receipt BEFORE the inner provider is touched. Missing/expired receipt,
// disallowed operation, stale lease generation, or an attempt to reach a
// different task or project all fail closed with zero provider traffic.
// ContextBoundProvider is IMMUTABLE after construction: every field is
// unexported, so no holder can swap the receipt, authority, or verifier
// past the constructor's checks, and the signature is RE-VERIFIED on every
// operation.
type ContextBoundProvider struct {
	inner     provider.TaskProvider
	ctx       TaskContext
	authority BindingAuthority
	verifier  *Verifier
	now       func() time.Time
	// currentGeneration is the newest lease generation known for this task
	// (interim source: the durable callback high-water mark; the canonical
	// claim/fence source lands with FAC-147). A receipt behind it authorizes
	// nothing — leased or not. Zero means "no newer authority known".
	currentGeneration int64
}

// NewContextBoundProvider AUTHENTICATES the receipt (signature verification
// against the published coordinator key — no unsigned context, however
// constructed, ever becomes a provider), validates it, and matches it
// against the canonical binding authority, so a foreign, forged, or
// mis-bound context can never be constructed into a usable provider.
func NewContextBoundProvider(inner provider.TaskProvider, tc TaskContext, authority BindingAuthority, verifier *Verifier, now func() time.Time, currentGeneration int64) (*ContextBoundProvider, error) {
	if inner == nil {
		return nil, fmt.Errorf("context-bound provider: nil inner provider")
	}
	if verifier == nil {
		return nil, fmt.Errorf("context-bound provider: no verifier — refusing unauthenticated receipts (FAC-145)")
	}
	if err := verifier.Verify(tc); err != nil {
		return nil, err
	}
	if err := tc.Validate(); err != nil {
		return nil, err
	}
	if err := authority.matches(tc); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &ContextBoundProvider{inner: inner, ctx: tc, authority: authority, verifier: verifier, now: now, currentGeneration: currentGeneration}, nil
}

// check is the single gate every operation routes through; the receipt is
// re-authenticated on EVERY call, not only at construction.
func (p *ContextBoundProvider) check(op provider.OpKind, taskID string) error {
	if p == nil || p.inner == nil {
		return fmt.Errorf("context-bound provider: nil provider")
	}
	if err := p.verifier.Verify(p.ctx); err != nil {
		return err
	}
	if err := p.ctx.Validate(); err != nil {
		return err
	}
	if err := p.authority.matches(p.ctx); err != nil {
		return err
	}
	if p.currentGeneration > p.ctx.LeaseGeneration {
		return fmt.Errorf("task context for %s holds stale generation %d (current %d) — refusing %s (FAC-145 fencing; canonical fence source arrives with FAC-147)",
			p.ctx.TaskRef, p.ctx.LeaseGeneration, p.currentGeneration, op)
	}
	if err := p.ctx.Authorize(p.now(), op); err != nil {
		return err
	}
	if taskID != "" && taskID != p.ctx.TaskID {
		return fmt.Errorf("task context for %s (task_id %s) cannot touch task %s (FAC-145: one receipt, one task)",
			p.ctx.TaskRef, p.ctx.TaskID, taskID)
	}
	return nil
}

func (p *ContextBoundProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	if err := p.check(provider.OpGet, id); err != nil {
		return nil, err
	}
	return p.inner.GetTask(ctx, id)
}

func (p *ContextBoundProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	if err := p.check(provider.OpList, ""); err != nil {
		return nil, err
	}
	if projectID != p.ctx.ProjectID {
		return nil, fmt.Errorf("task context for %s is bound to project %s, not %s (FAC-145: focused workspace state cannot redirect a provider read)",
			p.ctx.TaskRef, p.ctx.ProjectID, projectID)
	}
	return p.inner.ListTasks(ctx, projectID, status)
}

func (p *ContextBoundProvider) CreateTask(context.Context, *provider.Task) (*provider.Task, error) {
	return nil, fmt.Errorf("CreateTask: context-bound providers cannot create cards")
}

func (p *ContextBoundProvider) ClaimTask(ctx context.Context, taskID, role string) error {
	if err := p.check(provider.OpMutate, taskID); err != nil {
		return err
	}
	return p.inner.ClaimTask(ctx, taskID, role)
}

func (p *ContextBoundProvider) UpdateStatus(ctx context.Context, taskID, status string) error {
	if err := p.check(provider.OpMutate, taskID); err != nil {
		return err
	}
	return p.inner.UpdateStatus(ctx, taskID, status)
}

func (p *ContextBoundProvider) AddComment(ctx context.Context, taskID, body string) error {
	if err := p.check(provider.OpComment, taskID); err != nil {
		return err
	}
	return p.inner.AddComment(ctx, taskID, body)
}

var _ provider.TaskProvider = (*ContextBoundProvider)(nil)

// ListComments forwards the optional CommentReader capability under the
// SAME receipt gate as every other read (FAC-145).
func (p *ContextBoundProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	if err := p.check(provider.OpGet, taskID); err != nil {
		return nil, err
	}
	reader, ok := p.inner.(provider.CommentReader)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support comment readback (FAC-145)", p.ctx.ProviderType)
	}
	return reader.ListComments(ctx, taskID)
}
