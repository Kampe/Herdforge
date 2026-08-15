package tui

import (
	"context"
	"time"

	"github.com/Kampe/Herdforge/pkg/budget"
	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-300: Narrow read-only query interfaces for the TUI REPL. Each interface
// exposes only the methods the REPL needs to render live state. Adapters wrap
// the concrete packages so the REPL never fabricates success — when a backend
// is absent or errors, the REPL surfaces that explicitly (offline mode).

// DaemonStatus is a point-in-time read of daemon health.
type DaemonStatus struct {
	State     string
	LastError string
	UpdatedAt time.Time
}

// StatusQuerier queries daemon health (read-only).
type StatusQuerier interface {
	QueryStatus() (DaemonStatus, error)
}

// TaskQuerier lists tasks from the provider (read-only).
type TaskQuerier interface {
	ListTasks(ctx context.Context, status string) ([]*provider.Task, error)
}

// BudgetSnapshot is a point-in-time budget read.
type BudgetSnapshot struct {
	SpentUSD  float64
	MaxUSD    float64
	Tokens    int64
	Exhausted bool
}

// BudgetQuerier queries budget state (read-only).
type BudgetQuerier interface {
	QueryBudget() (BudgetSnapshot, error)
}

// ClaimInfo is a summarized lease for REPL display.
type ClaimInfo struct {
	Ref       string
	OwnerID   string
	Role      string
	Status    string
	ExpiresAt time.Time
}

// ClaimQuerier queries active claims (read-only).
type ClaimQuerier interface {
	ActiveClaims(ctx context.Context) ([]ClaimInfo, error)
	IsClaimed(ctx context.Context, ref string) (bool, error)
}

// LaneInfo is a summarized lane for REPL display.
type LaneInfo struct {
	Name     string
	Role     string
	Model    string
	Standing bool
}

// FleetQuerier queries configured lanes and live fleet status (read-only).
type FleetQuerier interface {
	Lanes() []LaneInfo
	FleetStatus() (string, error)
}

// Backend aggregates the narrow interfaces the REPL consults. A nil interface
// field means that subsystem is offline; the REPL labels its output
// accordingly instead of fabricating data.
type Backend struct {
	Status StatusQuerier
	Tasks  TaskQuerier
	Budget BudgetQuerier
	Claims ClaimQuerier
	Fleet  FleetQuerier
	Cfg    *config.Config
}

// OfflineBackend returns a Backend with every interface nil, representing
// explicit read-only offline mode.
func OfflineBackend() *Backend {
	return &Backend{}
}

// IsOffline reports whether the backend has no live query interfaces at all.
func (b *Backend) IsOffline() bool {
	if b == nil {
		return true
	}
	return b.Status == nil && b.Tasks == nil && b.Budget == nil && b.Claims == nil && b.Fleet == nil
}

// --- Concrete Adapters ---

// DaemonAdapter wraps a *daemon.Engine to satisfy StatusQuerier.
type DaemonAdapter struct {
	Engine *daemon.Engine
}

func (a DaemonAdapter) QueryStatus() (DaemonStatus, error) {
	if a.Engine == nil {
		return DaemonStatus{}, ErrOffline
	}
	h := a.Engine.ProviderHealth()
	return DaemonStatus{
		State:     string(h.State),
		LastError: h.LastError,
		UpdatedAt: h.UpdatedAt,
	}, nil
}

// ProviderAdapter wraps a provider.TaskProvider and config to satisfy
// TaskQuerier.
type ProviderAdapter struct {
	Prov       provider.TaskProvider
	ProjectID  string
}

func (a ProviderAdapter) ListTasks(ctx context.Context, status string) ([]*provider.Task, error) {
	if a.Prov == nil {
		return nil, ErrOffline
	}
	return a.Prov.ListTasks(ctx, a.ProjectID, status)
}

// BudgetAdapter wraps a *budget.BudgetManager to satisfy BudgetQuerier.
type BudgetAdapter struct {
	Manager *budget.BudgetManager
}

func (a BudgetAdapter) QueryBudget() (BudgetSnapshot, error) {
	if a.Manager == nil {
		return BudgetSnapshot{}, ErrOffline
	}
	return BudgetSnapshot{
		SpentUSD:  a.Manager.TotalCostUSD,
		MaxUSD:    a.Manager.MaxBudgetUSD,
		Tokens:    a.Manager.TotalTokens,
		Exhausted: a.Manager.IsExhausted(),
	}, nil
}

// ClaimAdapter wraps a *claim.ClaimManager to satisfy ClaimQuerier.
type ClaimAdapter struct {
	Manager *claim.ClaimManager
	Repo    string
	Prov    string
	Project string
}

func (a ClaimAdapter) ActiveClaims(ctx context.Context) ([]ClaimInfo, error) {
	if a.Manager == nil {
		return nil, ErrOffline
	}
	leases, err := a.Manager.ActiveClaims(ctx)
	if err != nil {
		return nil, err
	}
	infos := make([]ClaimInfo, 0, len(leases))
	for _, l := range leases {
		infos = append(infos, ClaimInfo{
			Ref:       l.TaskRef,
			OwnerID:   l.OwnerID,
			Role:      l.Role,
			Status:    string(l.Status),
			ExpiresAt: l.ExpiresAt,
		})
	}
	return infos, nil
}

func (a ClaimAdapter) IsClaimed(ctx context.Context, ref string) (bool, error) {
	if a.Manager == nil {
		return false, ErrOffline
	}
	return a.Manager.IsClaimed(ctx, claim.LeaseKey{
		Repo:     a.Repo,
		Provider: a.Prov,
		Project:  a.Project,
		TaskRef:  ref,
	})
}

// ConfigFleetAdapter projects configured lanes and fleet label from a Config.
// It satisfies FleetQuerier without requiring a live herdr process.
type ConfigFleetAdapter struct {
	Cfg       *config.Config
	FleetLabel string
	FleetErr   error
}

func (a ConfigFleetAdapter) Lanes() []LaneInfo {
	if a.Cfg == nil {
		return nil
	}
	lanes := make([]LaneInfo, 0, len(a.Cfg.Lanes))
	for _, l := range a.Cfg.Lanes {
		lanes = append(lanes, LaneInfo{
			Name:     l.Name,
			Role:     l.Role,
			Model:    l.Model,
			Standing: l.Standing,
		})
	}
	return lanes
}

func (a ConfigFleetAdapter) FleetStatus() (string, error) {
	if a.FleetErr != nil {
		return "", a.FleetErr
	}
	if a.FleetLabel != "" {
		return a.FleetLabel, nil
	}
	return "unknown", nil
}

// ErrOffline is returned by adapters when their underlying dependency is nil.
var ErrOffline = offlineErr{}

type offlineErr struct{}

func (offlineErr) Error() string { return "offline: no live backend connected" }

// IsOfflineErr reports whether err is an offline sentinel.
func IsOfflineErr(err error) bool {
	_, ok := err.(offlineErr)
	return ok
}
