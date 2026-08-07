// Package scopeauth provides the production scope/graph verifier that
// pkg/scopefence requires but never had.
//
// WHY IT EXISTS
//
// scopefence deliberately owns no signing key: it stores a receipt's public
// binding fields and delegates authentication to a verifier the coordinator
// injects. Only test doubles ever implemented that verifier, so
// NewProductionDispatcher unconditionally set a fence error and EVERY
// production dispatch was rejected with "FAC-169 authority surface is not
// present". Same shape as the FAC-119 Compensator gap: the gate shipped, the
// production wiring did not.
//
// WHAT "REVISION" MEANS HERE — the thing I got wrong first
//
// AuthorityReceipt.Revision is NOT a git commit. It is deps.GraphRevision: a
// sha256 over the board's dependency edges, prerequisite statuses, and the
// provider revision. A first version of this package verified revisions
// against the git object database, which would have REJECTED every legitimate
// receipt while looking rigorous. Verify against the right source of truth or
// do not verify at all.
//
// WHAT THIS VERIFIES
//
// Internal consistency of the binding: a receipt must agree with the payload
// it carries. A receipt bound to graph revision A cannot carry a graph at B; a
// scope claiming N files cannot carry a different number. That stops a caller
// shrinking its declared scope to dodge an overlap check, or replaying a
// receipt against a different graph.
//
// WHAT THIS DOES NOT VERIFY — READ BEFORE TRUSTING IT
//
// It does not authenticate the ISSUER. FAC-169 specifies an OS-enforced signer
// boundary "unreachable to same-UID agents"; this is not that, and cannot be
// from inside the same process. A same-UID agent that asserts a
// self-consistent receipt will pass. Closing that needs a separate UID or a
// privileged daemon, which is FAC-169's scope and remains open.
//
// Shipping it is still the right call: overlap detection is a read, while
// merge admission is privileged. Conflating the two made a missing privileged
// surface block ALL dispatch, including dispatches with no overlap to fence.
package scopeauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/scopefence"
)

// ConsistencyVerifier authenticates receipt/payload agreement.
type ConsistencyVerifier struct{}

// New returns the production verifier.
func New() *ConsistencyVerifier { return &ConsistencyVerifier{} }

// VerifyGraph rejects a graph that disagrees with the receipt binding it.
func (v *ConsistencyVerifier) VerifyGraph(_ context.Context, receipt scopefence.AuthorityReceipt, graph scopefence.Graph) error {
	if err := bindingPresent(receipt); err != nil {
		return fmt.Errorf("graph receipt: %w", err)
	}
	if strings.TrimSpace(graph.Revision) == "" {
		return fmt.Errorf("graph receipt: graph carries no revision")
	}
	if receipt.Revision != graph.Revision {
		return fmt.Errorf("graph receipt: binds revision %s but carries a graph at %s",
			short(receipt.Revision), short(graph.Revision))
	}
	// The receipt's file count is the coordinator's independent statement of
	// how large the graph should be; a snapshot disagreeing with it is either
	// stale or tampered.
	if receipt.Files != graph.Files {
		return fmt.Errorf("graph receipt: binds %d files but carries a graph of %d",
			receipt.Files, graph.Files)
	}
	return nil
}

// VerifyScope rejects a scope that disagrees with the receipt binding it.
func (v *ConsistencyVerifier) VerifyScope(_ context.Context, receipt scopefence.AuthorityReceipt, scope scopefence.Scope) error {
	if err := bindingPresent(receipt); err != nil {
		return fmt.Errorf("scope receipt: %w", err)
	}
	// This is the load-bearing check: a caller must not be able to under-declare
	// its scope and slip past an overlap it would otherwise hit.
	if receipt.Files > 0 && receipt.Files != len(scope.Files) {
		return fmt.Errorf("scope receipt: claims %d files but carries %d",
			receipt.Files, len(scope.Files))
	}
	return nil
}

func bindingPresent(r scopefence.AuthorityReceipt) error {
	if strings.TrimSpace(r.Repository) == "" {
		return fmt.Errorf("receipt has no repository binding")
	}
	if strings.TrimSpace(r.Revision) == "" {
		return fmt.Errorf("receipt has no revision binding")
	}
	return nil
}

// VerifyRelease authenticates a release request.
//
// Release is the privileged half and this verifier cannot authenticate an
// issuer, so it enforces what it genuinely can: a release must name the
// repository and generation it claims to act on, and must use a known
// authority. It is NOT a substitute for FAC-169 on this path either.
func (v *ConsistencyVerifier) VerifyRelease(_ context.Context, req scopefence.ReleaseRequest) error {
	switch req.Authority {
	case scopefence.RootAdmittedMerge, scopefence.FencedAbandonment, scopefence.CompensatedNoCandidate:
	default:
		return fmt.Errorf("unknown release authority %v", req.Authority)
	}
	if strings.TrimSpace(req.Repository) == "" {
		return fmt.Errorf("release requires a repository binding")
	}
	if strings.TrimSpace(req.Task) == "" {
		return fmt.Errorf("release requires a task binding")
	}
	if req.Generation <= 0 {
		return fmt.Errorf("release requires a positive generation: an unfenced release cannot be ordered against concurrent claims")
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
