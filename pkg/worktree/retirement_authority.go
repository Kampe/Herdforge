package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// SlotRetirementAuthority is implemented by anything that can positively
// authorize the destructive removal of slots under a pool root. An authority
// must refuse absent, ambiguous, unknown, or out-of-scope evidence; refusal
// is the fail-closed default, never a guess of safety.

// NewManifestFirstRetirementAuthority composes a manifest authority and a
// fallback authority with deliberate precedence: a pool root the manifest
// registry NAMES gets the manifest's verdict, final -- active or
// unconfirmed-complete retirement evidence protects it (retains), a fully
// completed generation authorizes it, and no other evidence path can
// overrule either. Only pools the manifest does not name at all fall
// through to the fallback (e.g. the native pool-creation authority), so a
// native-created pool can never bypass the review-retirement protection.
// Evidence read errors refuse fail-closed.
func NewManifestFirstRetirementAuthority(manifest, fallback SlotRetirementAuthority) *ManifestFirstRetirementAuthority {
	return &ManifestFirstRetirementAuthority{Manifest: manifest, Fallback: fallback}
}

// ManifestFirstRetirementAuthority gives the manifest authority the final
// verdict for any pool root it names.
type ManifestFirstRetirementAuthority struct {
	Manifest SlotRetirementAuthority
	Fallback SlotRetirementAuthority
}

// AuthorizePoolRoot implements the precedence contract above.
func (a *ManifestFirstRetirementAuthority) AuthorizePoolRoot(poolRootAbs string) error {
	if a == nil || a.Manifest == nil {
		return fmt.Errorf("pool root %s: no retirement authority configured, refusing", poolRootAbs)
	}
	if knower, ok := a.Manifest.(interface{ PoolRootKnown(string) (bool, error) }); ok {
		known, err := knower.PoolRootKnown(poolRootAbs)
		if err != nil {
			return fmt.Errorf("pool root %s: retirement evidence unreadable: %w, refusing", poolRootAbs, err)
		}
		if known {
			return a.Manifest.AuthorizePoolRoot(poolRootAbs)
		}
	}
	if a.Fallback == nil {
		return a.Manifest.AuthorizePoolRoot(poolRootAbs)
	}
	return a.Fallback.AuthorizePoolRoot(poolRootAbs)
}

// NativePoolCreationAuthority positively authorizes a pool root the owner's
// own native tooling demonstrably created and manages in THIS repository.
// The actual native authority evidence is: a schema-valid native pool.json
// (version 1, bounded read) whose every recorded slot is bound to an actual
// git registration under this repository root, with the pool root itself
// inside the symlink-resolved repository identity. A foreign, corrupt,
// unknown, or out-of-scope pool root retains -- deliberate fail-closed
// refusal, never disposable by inference.
type NativePoolCreationAuthority struct {
	// RepoRoot is the repository root the pool must be natively bound to.
	RepoRoot string
	// rootIdentity is the symlink-resolved repository root used for
	// containment decisions, resolved once at construction.
	rootIdentity string
}

// NewNativePoolCreationAuthority builds the authority for pool roots the
// native Pool primitives (NewPool/Ensure) created under repoRoot.
func NewNativePoolCreationAuthority(repoRoot string) *NativePoolCreationAuthority {
	identity, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		identity = filepath.Clean(repoRoot)
	}
	return &NativePoolCreationAuthority{RepoRoot: repoRoot, rootIdentity: identity}
}

// AuthorizePoolRoot implements SlotRetirementAuthority on native owner
// evidence: the pool root must carry a valid native pool.json whose slots
// are registered worktrees of this repository.
func (a *NativePoolCreationAuthority) AuthorizePoolRoot(poolRootAbs string) error {
	resolved, err := filepath.EvalSymlinks(poolRootAbs)
	if err != nil {
		return fmt.Errorf("pool root %s does not resolve natively: %w, refusing", poolRootAbs, err)
	}
	resolved = filepath.Clean(resolved)
	if !withinIdentity(a.rootIdentity, resolved) {
		return fmt.Errorf("pool root %s is outside the repository identity %s, refusing", resolved, a.rootIdentity)
	}
	data, err := readBoundedFile(filepath.Join(resolved, "pool.json"), maxPoolJSONBytes)
	if err != nil {
		return fmt.Errorf("pool root %s carries no valid native pool.json (%v); unknown-scope pools are not automatically disposable, refusing", resolved, err)
	}
	var state poolState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("pool root %s pool.json is not native-schema (%v); refusing", resolved, err)
	}
	if state.Version < 1 {
		return fmt.Errorf("pool root %s pool.json version %d is not a native pool state, refusing", resolved, state.Version)
	}
	registered, err := registeredWorktrees(context.Background(), a.RepoRoot)
	if err != nil {
		return fmt.Errorf("pool root %s: repository worktree registrations unreadable: %w, refusing", resolved, err)
	}
	for _, slot := range state.Slots {
		slotPath := filepath.Clean(slot.Path)
		// Slot paths were recorded under whatever spelling the creator
		// used (e.g. /var/... vs /private/var/... on macOS). Normalize
		// best-effort to the resolved identity before containment and
		// registration checks, and accept either spelling as the binding.
		resolvedSlotPath := slotPath
		if resolvedSlot, err := filepath.EvalSymlinks(slotPath); err == nil {
			resolvedSlotPath = filepath.Clean(resolvedSlot)
		}
		if !withinIdentity(resolved, slotPath) && !withinIdentity(resolved, resolvedSlotPath) {
			return fmt.Errorf("pool root %s records slot %s outside itself, refusing", resolved, slot.Name)
		}
		if _, registeredRaw := registered[slotPath]; !registeredRaw {
			if _, registeredResolved := registered[resolvedSlotPath]; !registeredResolved {
				return fmt.Errorf("pool root %s records slot %s with no git registration in this repository; its native state is not bound here, refusing", resolved, slot.Name)
			}
		}
	}
	return nil
}

// withinIdentity reports whether path is the identity itself or lies under
// it. Both arguments must already be cleaned absolute paths.
func withinIdentity(identity, path string) bool {
	if identity == path {
		return true
	}
	rel, err := filepath.Rel(identity, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
