package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// LeaseCapability is the durable worker-session authority written into the
// task worktree after atomic handoff. Written once with final owner+receipt.
type LeaseCapability struct {
	OwnerID     string    `json:"owner_id"`
	Generation  int64     `json:"generation"`
	TaskRef     string    `json:"task_ref"`
	Repo        string    `json:"repo"`
	Provider    string    `json:"provider"`
	Project     string    `json:"project"`
	TabID       string    `json:"tab_id,omitempty"`
	PaneID      string    `json:"pane_id,omitempty"`
	AgentName   string    `json:"agent_name,omitempty"`
	Receipt     string    `json:"receipt,omitempty"`
	Challenge   string    `json:"challenge,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	WrittenAt   time.Time `json:"written_at"`
	ClaimDBHint string    `json:"claim_db_hint,omitempty"`
}

// LeaseAck is the worker-written acknowledgement of the single capability.
type LeaseAck struct {
	OwnerID    string    `json:"owner_id"`
	Generation int64     `json:"generation"`
	Receipt    string    `json:"receipt"`
	Challenge  string    `json:"challenge"`
	AckedAt    time.Time `json:"acked_at"`
}

// HandbackDoneReceipt is a generation-bound durable local handback receipt.
type HandbackDoneReceipt struct {
	OwnerID    string    `json:"owner_id"`
	Generation int64     `json:"generation"`
	TaskRef    string    `json:"task_ref"`
	Receipt    string    `json:"receipt"`
	Challenge  string    `json:"challenge"`
	ReleasedAt time.Time `json:"released_at"`
}

// HandbackIntent is written BEFORE remote Release so a crash after remote
// success can complete local durability without re-releasing (idempotent).
type HandbackIntent struct {
	OwnerID        string    `json:"owner_id"`
	Generation     int64     `json:"generation"`
	TaskRef        string    `json:"task_ref"`
	Repo           string    `json:"repo"`
	Provider       string    `json:"provider"`
	Project        string    `json:"project"`
	Receipt        string    `json:"receipt"`
	Challenge      string    `json:"challenge"`
	IntentAt       time.Time `json:"intent_at"`
	RemoteReleased bool      `json:"remote_released"`
}

// LaunchSession distinguishes never-launched from lost authority (pt5t7 #2).
type LaunchSession struct {
	OwnerID     string    `json:"owner_id"`
	Generation  int64     `json:"generation"`
	TaskRef     string    `json:"task_ref"`
	Repo        string    `json:"repo"`
	Provider    string    `json:"provider"`
	Project     string    `json:"project"`
	Receipt     string    `json:"receipt"`
	Challenge   string    `json:"challenge"`
	LaunchedAt  time.Time `json:"launched_at"`
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

const (
	CapabilityFileName = ".herd/lease-capability.json"
	AckFileName        = ".herd/lease-ack.json"
	HandbackNoteName   = ".herd/LEASE-HANDBACK.md"
	HandbackDoneName   = ".herd/lease-handback.done"
	HandbackIntentName = ".herd/lease-handback.intent"
	SessionFileName    = ".herd/lease-session.json"
	capabilityLeaf     = "lease-capability.json"
	ackLeaf            = "lease-ack.json"
	handbackDoneLeaf   = "lease-handback.done"
	handbackIntentLeaf = "lease-handback.intent"
	sessionLeaf        = "lease-session.json"
)

// WriteLeaseCapability writes capability + launch session for a NEW handoff.
// Clears stale done/intent/ack only when generation changes. Same generation
// refresh must use RefreshLeaseCapabilityExpiry (heartbeat) so terminal
// recovery evidence is never erased.
func WriteLeaseCapability(worktreePath string, cap LeaseCapability) error {
	if cap.OwnerID == "" || cap.Generation <= 0 {
		return fmt.Errorf("provider: capability requires owner and generation")
	}
	if cap.Receipt == "" || cap.Receipt == "pending-receipt" {
		return fmt.Errorf("provider: capability requires final non-provisional receipt")
	}
	if cap.Challenge == "" {
		return fmt.Errorf("provider: capability requires challenge nonce")
	}
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()

	// Detect generation change vs same-gen rewrite.
	prevGen := int64(0)
	prevOwner := ""
	if raw, rerr := h.read(capabilityLeaf); rerr == nil {
		var prev LeaseCapability
		if json.Unmarshal(raw, &prev) == nil {
			prevGen = prev.Generation
			prevOwner = prev.OwnerID
		}
	}
	newGeneration := prevGen == 0 || prevGen != cap.Generation || prevOwner != cap.OwnerID
	if newGeneration {
		for _, leaf := range []string{handbackDoneLeaf, handbackIntentLeaf, ackLeaf} {
			if err := h.remove(leaf); err != nil {
				return fmt.Errorf("provider: clear stale %s: %w", leaf, err)
			}
		}
	}
	cap.WrittenAt = time.Now().UTC()
	raw, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return err
	}
	if err := h.atomicWrite(capabilityLeaf, raw, 0o600); err != nil {
		return err
	}
	if !newGeneration {
		// Same gen rewrite (should use RefreshLeaseCapabilityExpiry): never
		// clobber a completed or in-flight session.
		return nil
	}
	// Preserve completed session for prior gens is already cleared above via new gen.
	sess := LaunchSession{
		OwnerID: cap.OwnerID, Generation: cap.Generation, TaskRef: cap.TaskRef,
		Repo: cap.Repo, Provider: cap.Provider, Project: cap.Project,
		Receipt: cap.Receipt, Challenge: cap.Challenge,
		LaunchedAt: time.Now().UTC(), Completed: false,
	}
	sraw, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(sessionLeaf, sraw, 0o600)
}

// RefreshLeaseCapabilityExpiry updates only ExpiresAt on an existing capability
// after a successful Renew. Never clears handback intent/done/session —
// heartbeat must not erase terminal recovery evidence (parallel audit).
func RefreshLeaseCapabilityExpiry(worktreePath string, owner string, gen int64, expiresAt time.Time) error {
	cap, err := ReadLeaseCapability(worktreePath)
	if err != nil {
		return err
	}
	if cap.OwnerID != owner || cap.Generation != gen {
		return fmt.Errorf("provider: heartbeat capability owner/gen mismatch")
	}
	// Refuse refresh if handback already completed for this gen.
	if HandbackDoneMatches(worktreePath, owner, gen) {
		return fmt.Errorf("provider: refuse heartbeat after handback.done for gen=%d", gen)
	}
	if sess, serr := ReadLaunchSession(worktreePath); serr == nil && sess != nil && sess.Completed &&
		sess.OwnerID == owner && sess.Generation == gen {
		return fmt.Errorf("provider: refuse heartbeat after completed session gen=%d", gen)
	}
	cap.ExpiresAt = expiresAt
	cap.WrittenAt = time.Now().UTC()
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	raw, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(capabilityLeaf, raw, 0o600)
}

// WriteHandbackNote writes LEASE-HANDBACK.md under openat-held .herd.
func WriteHandbackNote(worktreePath string) error {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	body := `# Lease handback (FAC-147)

Capability at .herd/lease-capability.json is the single authority file.

1. ACK once with exact owner_id, generation, receipt, and challenge.
2. On BUILD COMPLETE or BUILD FAIL: herd lease-handback (mandatory, fail-closed).
3. Heartbeat: herd lease-heartbeat
`
	return h.atomicWrite("LEASE-HANDBACK.md", []byte(body), 0o644)
}

// WriteLeaseAck is the worker-side acknowledgement of the single capability.
func WriteLeaseAck(worktreePath string, ack LeaseAck) error {
	if ack.OwnerID == "" || ack.Generation <= 0 || ack.Challenge == "" {
		return fmt.Errorf("provider: ack requires owner, generation, challenge")
	}
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	ack.AckedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(ackLeaf, raw, 0o600)
}

// WriteHandbackDone writes a generation-bound durable receipt + parent fsync.
func WriteHandbackDone(worktreePath string, rec HandbackDoneReceipt) error {
	if rec.OwnerID == "" || rec.Generation <= 0 {
		return fmt.Errorf("provider: handback done requires owner and generation")
	}
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	if rec.ReleasedAt.IsZero() {
		rec.ReleasedAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(handbackDoneLeaf, raw, 0o644)
}

// ReadHandbackDone loads and validates a generation-bound done receipt.
func ReadHandbackDone(worktreePath string) (*HandbackDoneReceipt, error) {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	raw, err := h.read(handbackDoneLeaf)
	if err != nil {
		return nil, err
	}
	var rec HandbackDoneReceipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		if s := string(raw); strings.Contains(s, "owner=") {
			return nil, fmt.Errorf("provider: legacy handback.done lacks generation-bound JSON receipt")
		}
		return nil, fmt.Errorf("provider: corrupt handback.done: %w", err)
	}
	if rec.OwnerID == "" || rec.Generation <= 0 {
		return nil, fmt.Errorf("provider: handback.done missing owner/generation")
	}
	return &rec, nil
}

// HandbackDoneMatches reports whether a valid done receipt matches gen/owner.
func HandbackDoneMatches(worktreePath, owner string, gen int64) bool {
	rec, err := ReadHandbackDone(worktreePath)
	if err != nil {
		return false
	}
	return rec.OwnerID == owner && rec.Generation == gen
}

// WriteHandbackIntent persists intent before remote Release (crash recovery).
func WriteHandbackIntent(worktreePath string, intent HandbackIntent) error {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	if intent.IntentAt.IsZero() {
		intent.IntentAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(handbackIntentLeaf, raw, 0o600)
}

// ReadHandbackIntent loads pending handback intent if any.
func ReadHandbackIntent(worktreePath string) (*HandbackIntent, error) {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	raw, err := h.read(handbackIntentLeaf)
	if err != nil {
		return nil, err
	}
	var intent HandbackIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return nil, fmt.Errorf("provider: corrupt handback intent: %w", err)
	}
	return &intent, nil
}

// ReadLaunchSession loads the durable launch session if present.
func ReadLaunchSession(worktreePath string) (*LaunchSession, error) {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	raw, err := h.read(sessionLeaf)
	if err != nil {
		return nil, err
	}
	var sess LaunchSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("provider: corrupt lease-session: %w", err)
	}
	if sess.OwnerID == "" || sess.Generation <= 0 {
		return nil, fmt.Errorf("provider: lease-session missing owner/generation")
	}
	return &sess, nil
}

// WriteLaunchSession persists session state (completion mark).
func WriteLaunchSession(worktreePath string, sess LaunchSession) error {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	raw, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return h.atomicWrite(sessionLeaf, raw, 0o600)
}

// ReadLeaseAck loads worker acknowledgement via openat O_NOFOLLOW.
func ReadLeaseAck(worktreePath string) (*LeaseAck, error) {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	raw, err := h.read(ackLeaf)
	if err != nil {
		return nil, err
	}
	var ack LeaseAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// VerifyLeaseAck checks ack matches the single capability challenge.
func VerifyLeaseAck(cap *LeaseCapability, ack *LeaseAck) error {
	if cap == nil || ack == nil {
		return fmt.Errorf("provider: nil capability or ack")
	}
	if ack.OwnerID != cap.OwnerID || ack.Generation != cap.Generation {
		return fmt.Errorf("provider: ack owner/gen mismatch")
	}
	if ack.Receipt != cap.Receipt {
		return fmt.Errorf("provider: ack receipt mismatch")
	}
	if ack.Challenge != cap.Challenge {
		return fmt.Errorf("provider: ack challenge mismatch")
	}
	return nil
}

// AwaitLeaseAck polls for a valid worker ack of the single capability.
func AwaitLeaseAck(worktreePath string, cap *LeaseCapability, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ack, err := ReadLeaseAck(worktreePath)
		if err == nil {
			if v := VerifyLeaseAck(cap, ack); v == nil {
				return nil
			} else {
				last = v
			}
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timeout waiting for lease ack")
	}
	return fmt.Errorf("provider: lease ack not received: %w", last)
}

// ReadLeaseCapability loads capability via openat (fail-closed on any error).
func ReadLeaseCapability(worktreePath string) (*LeaseCapability, error) {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	raw, err := h.read(capabilityLeaf)
	if err != nil {
		return nil, err
	}
	var cap LeaseCapability
	if err := json.Unmarshal(raw, &cap); err != nil {
		return nil, fmt.Errorf("provider: corrupt capability: %w", err)
	}
	if cap.OwnerID == "" || cap.Generation <= 0 {
		return nil, fmt.Errorf("provider: invalid capability file")
	}
	return &cap, nil
}

// CapabilityPresent reports whether a valid capability file exists.
func CapabilityPresent(worktreePath string) (bool, error) {
	_, err := ReadLeaseCapability(worktreePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Heartbeat renews the lease under the worker session owner.
func Heartbeat(ctx context.Context, mgr *claim.ClaimManager, key claim.LeaseKey, ownerID string, generation int64) (*claim.Lease, error) {
	if mgr == nil {
		return nil, fmt.Errorf("provider: Heartbeat requires ClaimManager")
	}
	return mgr.Renew(ctx, key, ownerID, generation)
}

// Handback releases the worker-held lease.
func Handback(ctx context.Context, mgr *claim.ClaimManager, key claim.LeaseKey, ownerID string, generation int64) error {
	if mgr == nil {
		return fmt.Errorf("provider: Handback requires ClaimManager")
	}
	return mgr.Release(ctx, key, ownerID, generation)
}

func isAlreadyReleased(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "not the current") ||
		strings.Contains(s, "stale generation") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "ErrStaleGeneration") ||
		strings.Contains(s, "ErrNotFound") ||
		errors.Is(err, claim.ErrStaleGeneration) ||
		errors.Is(err, claim.ErrNotFound)
}

// TryAutoHandback is receipt-driven and fail-closed on lost authority (pt5t7 #2):
// launched sessions require matching generation-bound done + remote release.
func TryAutoHandback(ctx context.Context, mgr *claim.ClaimManager, worktreePath string) error {
	if mgr == nil {
		return fmt.Errorf("provider: TryAutoHandback requires ClaimManager")
	}

	if intent, err := ReadHandbackIntent(worktreePath); err == nil && intent != nil {
		return completeHandbackFromIntent(ctx, mgr, worktreePath, intent)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider: handback intent read: %w", err)
	}

	cap, err := ReadLeaseCapability(worktreePath)
	if err != nil {
		if os.IsNotExist(err) {
			return handbackWithoutCapability(ctx, mgr, worktreePath)
		}
		return fmt.Errorf("provider: capability read fail-closed: %w", err)
	}

	if HandbackDoneMatches(worktreePath, cap.OwnerID, cap.Generation) {
		return cleanupAfterDone(worktreePath, cap.OwnerID, cap.Generation)
	}

	intent := HandbackIntent{
		OwnerID: cap.OwnerID, Generation: cap.Generation, TaskRef: cap.TaskRef,
		Repo: cap.Repo, Provider: cap.Provider, Project: cap.Project,
		Receipt: cap.Receipt, Challenge: cap.Challenge, IntentAt: time.Now().UTC(),
	}
	if err := WriteHandbackIntent(worktreePath, intent); err != nil {
		return fmt.Errorf("provider: write handback intent: %w", err)
	}
	return completeHandbackFromIntent(ctx, mgr, worktreePath, &intent)
}

// handbackWithoutCapability: session record decides never-launched vs lost authority.
func handbackWithoutCapability(ctx context.Context, mgr *claim.ClaimManager, worktreePath string) error {
	sess, serr := ReadLaunchSession(worktreePath)
	if serr != nil && !os.IsNotExist(serr) {
		return fmt.Errorf("provider: session read fail-closed: %w", serr)
	}
	if sess == nil || os.IsNotExist(serr) {
		// Truly never launched — no session, no capability.
		if _, derr := ReadHandbackDone(worktreePath); derr == nil {
			// Orphan done without session is ok (idempotent complete).
			return nil
		} else if derr != nil && !os.IsNotExist(derr) {
			return fmt.Errorf("provider: handback.done invalid with no session: %w", derr)
		}
		return nil
	}
	if sess.Completed {
		if !HandbackDoneMatches(worktreePath, sess.OwnerID, sess.Generation) {
			return fmt.Errorf("provider: session completed but done receipt missing/mismatch gen=%d", sess.Generation)
		}
		// Remote lease must not still be held by this generation.
		if err := assertLeaseReleased(ctx, mgr, sess); err != nil {
			return err
		}
		return nil
	}
	// Incomplete session + no capability = lost authority (pt5t7 #2).
	// Try intent recovery first.
	if intent, err := ReadHandbackIntent(worktreePath); err == nil && intent != nil {
		return completeHandbackFromIntent(ctx, mgr, worktreePath, intent)
	}
	// Reconstruct intent from session and finish (remote may still be held).
	intent := HandbackIntent{
		OwnerID: sess.OwnerID, Generation: sess.Generation, TaskRef: sess.TaskRef,
		Repo: sess.Repo, Provider: sess.Provider, Project: sess.Project,
		Receipt: sess.Receipt, Challenge: sess.Challenge, IntentAt: time.Now().UTC(),
	}
	if err := WriteHandbackIntent(worktreePath, intent); err != nil {
		return fmt.Errorf("provider: recover intent from session: %w", err)
	}
	return completeHandbackFromIntent(ctx, mgr, worktreePath, &intent)
}

func assertLeaseReleased(ctx context.Context, mgr *claim.ClaimManager, sess *LaunchSession) error {
	if mgr == nil || sess == nil {
		return nil
	}
	key := claim.LeaseKey{Repo: sess.Repo, Provider: sess.Provider, Project: sess.Project, TaskRef: sess.TaskRef}
	// Release is idempotent if already gone; success/already-released both ok.
	if err := mgr.Release(ctx, key, sess.OwnerID, sess.Generation); err != nil && !isAlreadyReleased(err) {
		return fmt.Errorf("provider: session complete but lease still held: %w", err)
	}
	return nil
}

func cleanupAfterDone(worktreePath, owner string, gen int64) error {
	if err := RemoveLeaseCapability(worktreePath); err != nil {
		return fmt.Errorf("provider: remove capability after done: %w", err)
	}
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := h.remove(handbackIntentLeaf); err != nil {
		return fmt.Errorf("provider: remove intent after done: %w", err)
	}
	if err := h.fsync(); err != nil {
		return fmt.Errorf("provider: fsync after done cleanup: %w", err)
	}
	// Mark session completed if present.
	if sess, err := ReadLaunchSession(worktreePath); err == nil && sess != nil {
		if sess.OwnerID == owner && sess.Generation == gen && !sess.Completed {
			sess.Completed = true
			sess.CompletedAt = time.Now().UTC()
			if err := WriteLaunchSession(worktreePath, *sess); err != nil {
				return fmt.Errorf("provider: mark session completed: %w", err)
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider: session read after done: %w", err)
	}
	return nil
}

func completeHandbackFromIntent(ctx context.Context, mgr *claim.ClaimManager, worktreePath string, intent *HandbackIntent) error {
	if intent == nil {
		return fmt.Errorf("provider: nil handback intent")
	}
	if HandbackDoneMatches(worktreePath, intent.OwnerID, intent.Generation) {
		return cleanupAfterDone(worktreePath, intent.OwnerID, intent.Generation)
	}
	key := claim.LeaseKey{
		Repo: intent.Repo, Provider: intent.Provider, Project: intent.Project, TaskRef: intent.TaskRef,
	}
	if cap, capErr := ReadLeaseCapability(worktreePath); capErr == nil && cap != nil {
		key = KeyFromCapability(cap)
	} else if capErr != nil && !os.IsNotExist(capErr) {
		return fmt.Errorf("provider: capability read during handback: %w", capErr)
	}
	if key.TaskRef == "" || key.Repo == "" {
		if intent.RemoteReleased {
			return finalizeLocalHandback(worktreePath, intent)
		}
		return fmt.Errorf("provider: handback incomplete: missing lease key material")
	}

	if !intent.RemoteReleased {
		if err := Handback(ctx, mgr, key, intent.OwnerID, intent.Generation); err != nil {
			if !isAlreadyReleased(err) {
				return fmt.Errorf("provider: handback release failed (intent retained): %w", err)
			}
		}
		intent.RemoteReleased = true
		if err := WriteHandbackIntent(worktreePath, *intent); err != nil {
			return fmt.Errorf("provider: persist RemoteReleased intent: %w", err)
		}
	}
	return finalizeLocalHandback(worktreePath, intent)
}

func finalizeLocalHandback(worktreePath string, intent *HandbackIntent) error {
	done := HandbackDoneReceipt{
		OwnerID: intent.OwnerID, Generation: intent.Generation, TaskRef: intent.TaskRef,
		Receipt: intent.Receipt, Challenge: intent.Challenge, ReleasedAt: time.Now().UTC(),
	}
	if err := WriteHandbackDone(worktreePath, done); err != nil {
		return fmt.Errorf("provider: handback done marker failed (capability retained): %w", err)
	}
	if err := RemoveLeaseCapability(worktreePath); err != nil {
		return fmt.Errorf("provider: capability removal failed after handback: %w", err)
	}
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := h.remove(handbackIntentLeaf); err != nil {
		return fmt.Errorf("provider: intent removal after handback: %w", err)
	}
	if err := h.fsync(); err != nil {
		return fmt.Errorf("provider: fsync after handback: %w", err)
	}
	// Readback: capability gone.
	if _, err := ReadLeaseCapability(worktreePath); err == nil {
		return fmt.Errorf("provider: capability still present after removal")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("provider: capability readback after removal: %w", err)
	}
	if !HandbackDoneMatches(worktreePath, intent.OwnerID, intent.Generation) {
		return fmt.Errorf("provider: handback.done readback validation failed")
	}
	// Mark session completed.
	sess := LaunchSession{
		OwnerID: intent.OwnerID, Generation: intent.Generation, TaskRef: intent.TaskRef,
		Repo: intent.Repo, Provider: intent.Provider, Project: intent.Project,
		Receipt: intent.Receipt, Challenge: intent.Challenge,
		LaunchedAt: time.Now().UTC(), Completed: true, CompletedAt: time.Now().UTC(),
	}
	if existing, err := ReadLaunchSession(worktreePath); err == nil && existing != nil {
		sess.LaunchedAt = existing.LaunchedAt
	}
	if err := WriteLaunchSession(worktreePath, sess); err != nil {
		return fmt.Errorf("provider: mark session completed: %w", err)
	}
	return nil
}

// RemoveLeaseCapability removes capability + ack via openat unlinkat.
func RemoveLeaseCapability(worktreePath string) error {
	h, err := openSecureHerd(worktreePath)
	if err != nil {
		return err
	}
	defer h.Close()
	for _, leaf := range []string{capabilityLeaf, ackLeaf} {
		if err := h.remove(leaf); err != nil {
			return fmt.Errorf("provider: remove %s: %w", leaf, err)
		}
	}
	if err := h.fsync(); err != nil {
		return fmt.Errorf("provider: fsync after capability removal: %w", err)
	}
	return nil
}

// KeyFromCapability rebuilds the lease key from a capability file.
func KeyFromCapability(cap *LeaseCapability) claim.LeaseKey {
	if cap == nil {
		return claim.LeaseKey{}
	}
	return claim.LeaseKey{
		Repo: cap.Repo, Provider: cap.Provider, Project: cap.Project, TaskRef: cap.TaskRef,
	}
}

// MintChallenge returns a random challenge nonce for the handshake.
func MintChallenge() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("provider: crypto/rand for lease challenge: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ParseHandbackDoneLegacyGen is for tests.
func ParseHandbackDoneLegacyGen(s string) (int64, error) {
	for _, part := range strings.Fields(s) {
		if strings.HasPrefix(part, "gen=") {
			return strconv.ParseInt(strings.TrimPrefix(part, "gen="), 10, 64)
		}
	}
	return 0, fmt.Errorf("no gen")
}

// Ensure filepath used (HandbackDoneName path helpers for external tools).
var _ = filepath.Join
