package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// brokerVerdictFixture builds the minimal broker world for verdict-retention
// checks: a signed reviewer receipt backed by a live lease, a memory provider
// with exact effect readback, and the real broker serve loop over a pipe.
type brokerVerdictFixture struct {
	root      string
	reviewer  dispatch.TaskContext
	memory    *provider.MemoryProvider
	signer    *dispatch.Signer
	leaseID   string
	candidate string
	resp      func(req brokerRequest) brokerResponse
}

func newBrokerVerdictFixture(t *testing.T, ref, candidate string) brokerVerdictFixture {
	t.Helper()
	keyDir, root := t.TempDir(), t.TempDir()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, "herdforge", root)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	memory := provider.NewMemoryProvider()
	memory.AddTask(&provider.Task{ID: "task-id-1", Ref: ref, Status: "in-review", ProjectID: "proj-x"})

	key := claim.LeaseKey{Repo: "herdforge", Provider: "memory", Project: "proj-x", TaskRef: ref}
	lease, aErr := leaseStore.Acquire(context.Background(), key, "coordinator-test", dispatch.RoleReviewer, "", time.Now(), time.Hour)
	if aErr != nil {
		t.Fatal(aErr)
	}
	tc := dispatch.TaskContext{
		ProviderType: "memory", ProjectID: "proj-x", Repository: "herdforge",
		Role: dispatch.RoleReviewer, TaskRef: ref, TaskID: "task-id-1", Branch: "herd/fac-740",
		BaseSHA: "ba5e1234cafe1234beefcafe1234beefcafe1234", CandidateSHA: candidate,
		LeaseID: fmt.Sprintf("claim:%d", lease.ID), LeaseGeneration: lease.Generation, LeaseTaskRef: ref,
		SessionID: "reviewer-fac740", AllowedOps: dispatch.ReviewerOps,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	signed, sErr := signer.Issue(tc)
	if sErr != nil {
		t.Fatal(sErr)
	}
	// Release the store handle: the broker opens its own connection by path.
	leaseStore.Close()

	auth := dispatch.BindingAuthority{Repository: "herdforge", ProviderType: "memory", ProjectID: "proj-x"}
	return brokerVerdictFixture{
		root:      root,
		reviewer:  signed,
		memory:    memory,
		signer:    signer,
		leaseID:   fmt.Sprintf("claim:%d", lease.ID),
		candidate: candidate,
		resp: func(req brokerRequest) brokerResponse {
			client, server := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveBrokerConn(server, root, &config.Config{}, auth, verifier, signer, memory)
			}()
			_ = client.SetDeadline(time.Now().Add(60 * time.Second))
			if err := json.NewEncoder(client).Encode(req); err != nil {
				t.Fatal(err)
			}
			var resp brokerResponse
			if err := json.NewDecoder(client).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			client.Close()
			<-done
			return resp
		},
	}
}

// TestBrokerVerdict_RetainsCanonicalInboxArtifact is the FAC-740 GREEN gate:
// a delivered broker verdict must leave a canonical review-inbox artifact
// (FAC-351 seam restored) — exact-SHA, receipt-bound reviewer, idempotent
// under retry, written BEFORE the consumable callback.
func TestBrokerVerdict_RetainsCanonicalInboxArtifact(t *testing.T) {
	candidate := "cafe1234beefcafe1234beefcafe1234beefcafe"
	fx := newBrokerVerdictFixture(t, "FAC-740", candidate)

	resp := fx.resp(brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer})
	if !resp.OK {
		t.Fatalf("verdict must pass: %s", resp.Error)
	}
	inbox := filepath.Join(fx.root, ".herd", "review", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatalf("canonical review inbox must exist after a delivered verdict (FAC-351): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("exactly one canonical artifact expected, got %d", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(entries[0].Name(), candidate[:12]+"-verdict-") {
		t.Fatalf("artifact must be candidate-keyed verdict evidence, got %s", entries[0].Name())
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("canonical artifact must be 0600, got %v", perm)
	}
	body, err := os.ReadFile(filepath.Join(inbox, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	artifact := string(body)
	for _, want := range []string{
		"sha: " + candidate,
		"reviewer: reviewer-fac740",
		"verdict: PASS",
		"reviewed-head: " + candidate,
		"task: FAC-740",
		"[effect verdict-delivered:herdforge:FAC-740:" + candidate + ":gen1:claim:",
	} {
		if !strings.Contains(artifact, want) {
			t.Fatalf("canonical artifact missing %q:\n%s", want, artifact)
		}
	}
	// Idempotency: a replayed identical verdict must not duplicate artifacts.
	resp = fx.resp(brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer})
	if !resp.OK {
		t.Fatalf("replayed verdict must stay converged: %s", resp.Error)
	}
	entries, err = os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("retention must be idempotent, got %d artifacts", len(entries))
	}
}

// TestBrokerVerdict_RetryAfterArtifactDelete_HealsCrashWindow is the FAC-351
// crash-window gate: a verdict delivered and read back, but lost before
// retention (or deleted afterwards), must be re-retained by the RETRY path —
// the durable bus marker means no second provider delivery, and the missing
// artifact is recreated exactly once instead of the retry reporting success
// past a missing seam.
func TestBrokerVerdict_RetryAfterArtifactDelete_HealsCrashWindow(t *testing.T) {
	candidate := "cafe1234beefcafe1234beefcafe1234beefcafe"
	fx := newBrokerVerdictFixture(t, "FAC-740", candidate)

	resp := fx.resp(brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer})
	if !resp.OK {
		t.Fatalf("first verdict must pass: %s", resp.Error)
	}
	inbox := filepath.Join(fx.root, ".herd", "review", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first verdict must retain exactly one artifact: %v %v", entries, err)
	}
	if err := os.Remove(filepath.Join(inbox, entries[0].Name())); err != nil {
		t.Fatal(err)
	}

	resp = fx.resp(brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer})
	if !resp.OK {
		t.Fatalf("retry after artifact loss must reconcile the crash window: %s", resp.Error)
	}
	entries, err = os.ReadDir(inbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry must re-retain exactly one canonical artifact: %v %v", entries, err)
	}
	// The bus must still carry exactly ONE delivered verdict (no redelivery).
	mb := mail.NewMailbox(mail.CallbackMailPath(fx.root))
	envs, err := mb.ReadInbox(mail.CoordinatorInbox)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, e := range envs {
		var cb mail.Callback
		if json.Unmarshal([]byte(e.Body), &cb) == nil && len(cb.DedupeID) >= len(mail.VerdictDeliveredPrefix) && cb.DedupeID[:len(mail.VerdictDeliveredPrefix)] == mail.VerdictDeliveredPrefix {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("retry must not redeliver, found %d delivered verdict records", delivered)
	}
}

// fxVerdictEffectID rebuilds the broker's exact effect identity for the
// fixture's verdict: the same composition serveBrokerConn uses.
func (fx brokerVerdictFixture) fxVerdictEffectID(verdict string) string {
	effect := fmt.Sprintf("%s:%s:%s:gen%d:%s:%s", "herdforge", fx.reviewer.TaskRef,
		fx.candidate, fx.reviewer.LeaseGeneration, fx.leaseID, verdict)
	return mail.VerdictEffectID(effect)
}

// seedVerdictClaim posts a VALID coordinator-signed ownership claim for the
// effect, exactly as winVerdictClaim composes one — the durable state a
// crashed attempt leaves behind between its claim and its delivery.
func (fx brokerVerdictFixture) seedVerdictClaim(t *testing.T, verdict string) {
	t.Helper()
	effectID := fx.fxVerdictEffectID(verdict)
	owner := "crashed-owner-nonce"
	sig, err := fx.signer.SignBytes([]byte("herd-verdict-claim:" + effectID + "\n" + owner))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.memory.AddComment(context.Background(), fx.reviewer.TaskID,
		fmt.Sprintf("%s%s owner=%s sig=%s]", "[verdict-claim ", effectID, owner, sig)); err != nil {
		t.Fatal(err)
	}
}

// fxSeedConflictingArtifact pre-seeds the canonical destination the broker's
// retention would use (same content-addressed name, different content), so
// retention fails with the exact duplicate-conflict refusal until the
// conflict is resolved.
func (fx brokerVerdictFixture) fxSeedConflictingArtifact(t *testing.T, verdict string) string {
	t.Helper()
	effectID := fx.fxVerdictEffectID(verdict)
	sum := sha256.Sum256([]byte(effectID))
	name := fx.candidate[:12] + "-verdict-" + hex.EncodeToString(sum[:6]) + ".md"
	dst := filepath.Join(fx.root, ".herd", "review", "inbox", name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("a DIFFERENT retained artifact for the same effect name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func (fx brokerVerdictFixture) deliveredCallbackCount(t *testing.T) int {
	t.Helper()
	mb := mail.NewMailbox(mail.CallbackMailPath(fx.root))
	envs, err := mb.ReadInbox(mail.CoordinatorInbox)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, e := range envs {
		var cb mail.Callback
		if json.Unmarshal([]byte(e.Body), &cb) == nil && strings.HasPrefix(cb.DedupeID, mail.VerdictDeliveredPrefix) {
			delivered++
		}
	}
	return delivered
}

// TestBrokerVerdict_ClaimedButUndelivered_RetryNeverReportsSuccess is the
// FAC-740 review finding 1 control (attempt 1): a verdict whose effect is
// claimed on the provider but never delivered must not converge on a silent
// OK — the claimant may have stalled between its claim marker and delivery,
// and an OK there strands the verdict with no canonical authority. Against
// the shipped candidate the retry returned OK with zero delivered callbacks;
// this assertion fails there.
func TestBrokerVerdict_ClaimedButUndelivered_RetryNeverReportsSuccess(t *testing.T) {
	candidate := "cafe1234beefcafe1234beefcafe1234beefcafe"
	fx := newBrokerVerdictFixture(t, "FAC-740", candidate)
	fx.seedVerdictClaim(t, "APPROVED")

	resp := fx.resp(brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer})
	if resp.OK {
		t.Fatal("a claimed-but-undelivered verdict must never report success without evidence")
	}
	if !strings.Contains(resp.Error, "not delivered") {
		t.Fatalf("refusal must say why:\n%s", resp.Error)
	}
	if n := fx.deliveredCallbackCount(t); n != 0 {
		t.Fatalf("no consumable callback may exist, found %d", n)
	}
}

// TestBrokerVerdict_RetentionConflictThenRetry_Reconciles is the exact
// stranding repro from the FAC-740 review: attempt 1 fails retention on a
// pre-seeded conflicting artifact (the provider already carries the effect
// and the claim marker stands); attempt 2 must NOT report OK while the
// delivered callback is still missing — it retries retention and fails with
// the same exact refusal. Once the conflict is resolved, the NEXT retry
// completes the convergence: artifact retained, delivered record posted,
// exactly once.
func TestBrokerVerdict_RetentionConflictThenRetry_Reconciles(t *testing.T) {
	candidate := "cafe1234beefcafe1234beefcafe1234beefcafe"
	fx := newBrokerVerdictFixture(t, "FAC-740", candidate)
	conflict := fx.fxSeedConflictingArtifact(t, "APPROVED")
	req := brokerRequest{Op: "verdict", Ref: "FAC-740", Body: "APPROVED", CandidateSHA: candidate, WorktreeHEAD: candidate, Receipt: fx.reviewer}

	resp := fx.resp(req)
	if resp.OK || !strings.Contains(resp.Error, "canonical review retention failed") {
		t.Fatalf("attempt 1 must fail retention exactly as before: %+v", resp)
	}
	resp = fx.resp(req)
	if resp.OK {
		t.Fatal("the retry must not report success while the delivered record is still missing")
	}
	if !strings.Contains(resp.Error, "canonical retention failed") {
		t.Fatalf("retry must surface the retention refusal:\n%s", resp.Error)
	}
	if n := fx.deliveredCallbackCount(t); n != 0 {
		t.Fatalf("no consumable callback may exist past a failed retention, found %d", n)
	}
	// Resolve the conflict: the next retry reconciles the whole seam.
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	resp = fx.resp(req)
	if !resp.OK {
		t.Fatalf("retry after conflict resolution must converge: %s", resp.Error)
	}
	if n := fx.deliveredCallbackCount(t); n != 1 {
		t.Fatalf("exactly one delivered record expected, found %d", n)
	}
	entries, err := os.ReadDir(filepath.Join(fx.root, ".herd", "review", "inbox"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("convergence must retain exactly one canonical artifact: %v %v", entries, err)
	}
}
