package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// RepoRoot is the canonical (absolute, symlink-resolved) repository
	// root the pool must be natively bound to. It anchors relative recorded
	// slot paths and the `git -C` registration read.
	RepoRoot string
	// rootIdentity is the canonical repository identity used for
	// containment decisions; it equals RepoRoot and is kept explicit at
	// use sites.
	rootIdentity string
}

// canonicalIdentity resolves a path to one absolute, symlink-resolved,
// cleaned identity. Relative inputs are anchored at the current process
// working directory -- which, for every native creation path (the
// documented CLI default RepoRoot=".", tests included), is the repository
// root. Every authority comparison (repository identity, pool root, recorded
// slot path, git registration key) is made between identities canonicalized
// by this one helper, so a relative default never loses to an absolute
// registration spelling or vice versa.
func canonicalIdentity(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// NewNativePoolCreationAuthority builds the authority for pool roots the
// native Pool primitives (NewPool/Ensure) created under repoRoot. The root
// is canonicalized once -- a relative default (".") binds to the actual
// repository the process runs in, not to a spelling.
func NewNativePoolCreationAuthority(repoRoot string) *NativePoolCreationAuthority {
	canonical := canonicalIdentity(repoRoot)
	return &NativePoolCreationAuthority{RepoRoot: canonical, rootIdentity: canonical}
}

// NewNativePoolCreationAuthorityAt is NewNativePoolCreationAuthority with an
// explicit pre-resolved root identity (used by tests that must not depend on
// process cwd).
func NewNativePoolCreationAuthorityAt(repoRoot string) *NativePoolCreationAuthority {
	return NewNativePoolCreationAuthority(repoRoot)
}

// AuthorizePoolRoot implements SlotRetirementAuthority on native owner
// evidence: the pool root must carry a valid native pool.json whose slots
// are registered worktrees of this repository.
func (a *NativePoolCreationAuthority) AuthorizePoolRoot(poolRootAbs string) error {
	resolved := canonicalIdentity(poolRootAbs)
	if _, statErr := os.Lstat(resolved); statErr != nil {
		return fmt.Errorf("pool root %s does not resolve natively: %v, refusing", poolRootAbs, statErr)
	}
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
	// Registration keys are canonicalized to the same identity every other
	// comparison uses; a registration recorded under an un-resolved spelling
	// (e.g. /var/... while the identity resolved to /private/var/...) still
	// binds.
	registeredIdentities := make(map[string]bool, len(registered))
	for key := range registered {
		registeredIdentities[canonicalIdentity(key)] = true
	}
	for _, slot := range state.Slots {
		// Recorded slot paths are spelled exactly as the creator spelled
		// the pool root: absolute, or relative to the process cwd at
		// creation (the repository root for every native path). Anchor
		// relative spellings at the canonical repository identity and
		// resolve both to one identity before comparing.
		slotSpelling := slot.Path
		if !filepath.IsAbs(slotSpelling) {
			slotSpelling = filepath.Join(a.rootIdentity, slotSpelling)
		}
		canonicalSlot := canonicalIdentity(slotSpelling)
		if !withinIdentity(resolved, canonicalSlot) {
			return fmt.Errorf("pool root %s records slot %s outside itself, refusing", resolved, slot.Name)
		}
		if !registeredIdentities[canonicalSlot] {
			return fmt.Errorf("pool root %s records slot %s with no git registration in this repository; its native state is not bound here, refusing", resolved, slot.Name)
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
