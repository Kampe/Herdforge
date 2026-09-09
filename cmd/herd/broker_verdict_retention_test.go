package main

import (
	"context"
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
	root     string
	reviewer dispatch.TaskContext
	resp     func(req brokerRequest) brokerResponse
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
		root:     root,
		reviewer: signed,
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
