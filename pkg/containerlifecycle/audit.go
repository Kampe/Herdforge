package containerlifecycle

import (
	"context"
	"sort"
)

// ContainerLister enumerates live containers for auditing. DockerListAll
// is the production implementation; tests fake it.
type ContainerLister func(ctx context.Context) ([]LiveContainer, error)

// AuditUnowned cross-references every live container against the
// receipt store and reports the ones with no receipt at all — either
// pre-existing (older than this store) or created outside it. It never
// removes anything: FAC-200 requires a human to independently audit
// ownership before any of these are cleaned up (e.g. the 18 FAC-174
// containers observed 2026-08-04).
func AuditUnowned(ctx context.Context, store *Store, list ContainerLister) ([]LiveContainer, error) {
	all, err := list(ctx)
	if err != nil {
		return nil, err
	}
	var unowned []LiveContainer
	for _, c := range all {
		r, err := store.Get(c.ID)
		if err != nil {
			return nil, err
		}
		if r == nil {
			unowned = append(unowned, c)
		}
	}
	sort.Slice(unowned, func(i, j int) bool { return unowned[i].ID < unowned[j].ID })
	return unowned, nil
}

// StatusReport is the fleet status evidence FAC-200 requires: owned
// active, owned terminal-awaiting-cleanup, removed with absence proved,
// quarantined, and unowned containers found live on the host. AuditError
// is set (rather than failing Status outright) when the live docker
// audit can't run — receipt-store status must stay available even on a
// host without docker.
type StatusReport struct {
	OwnedActive          []Receipt       `json:"owned_active"`
	OwnedAwaitingCleanup []Receipt       `json:"owned_awaiting_cleanup"`
	Removed              []Receipt       `json:"removed"`
	Quarantined          []Receipt       `json:"quarantined"`
	Unowned              []LiveContainer `json:"unowned,omitempty"`
	AuditError           string          `json:"audit_error,omitempty"`
}

// Status builds a StatusReport from the receipt store and a live audit.
func Status(ctx context.Context, store *Store, list ContainerLister) (StatusReport, error) {
	all, err := store.ListAll()
	if err != nil {
		return StatusReport{}, err
	}
	var report StatusReport
	for _, r := range all {
		switch r.State {
		case StateRegistered, StateStarted:
			report.OwnedActive = append(report.OwnedActive, r)
		case StateAwaitingCleanup:
			report.OwnedAwaitingCleanup = append(report.OwnedAwaitingCleanup, r)
		case StateRemoved:
			report.Removed = append(report.Removed, r)
		case StateQuarantined:
			report.Quarantined = append(report.Quarantined, r)
		}
	}
	unowned, err := AuditUnowned(ctx, store, list)
	if err != nil {
		report.AuditError = err.Error()
		return report, nil
	}
	report.Unowned = unowned
	return report, nil
}
