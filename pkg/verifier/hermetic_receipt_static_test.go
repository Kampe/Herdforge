package verifier

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Pure fake cases cover forged checksums, signature mutation, time/policy
// bypasses, isolation cardinality, replay duplicate/conflict/failure, and
// callback ordering. No fixture starts a process or touches the host.
func TestHermeticReceiptAdversarialFixtures(t *testing.T) {
	base, req, verifier, signer := staticHermeticFixture()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*HermeticReceiptV1, *HermeticAdmissionRequest)
	}{
		{"forged-recomputed-checksum-invalid-signature", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) {
			r.CandidateSHA = strings.Repeat("b", 40)
			req.CandidateSHA = r.CandidateSHA
			r.PayloadDigest = payloadDigest(*r)
		}},
		{"signature-field-mutation", func(r *HermeticReceiptV1, _ *HermeticAdmissionRequest) { r.Signature[0] ^= 1 }},
		{"future-start", func(r *HermeticReceiptV1, _ *HermeticAdmissionRequest) {
			r.StartedAt = now.Add(time.Minute)
			resignStatic(r, signer)
		}},
		{"overlong-ttl", func(r *HermeticReceiptV1, _ *HermeticAdmissionRequest) {
			r.ExpiresAt = r.StartedAt.Add(HermeticReceiptMaxTTL + time.Nanosecond)
			resignStatic(r, signer)
		}},
		{"empty-field-bypass", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) { req.SourceCopyDigest = "" }},
		{"zero-id-bypass", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) {
			r.UID = 0
			r.GID = 0
			req.UID = 0
			req.GID = 0
			resignStatic(r, signer)
		}},
		{"both-isolation-identities", func(r *HermeticReceiptV1, _ *HermeticAdmissionRequest) {
			r.Isolation.VMIdentity = "vm:also"
			resignStatic(r, signer)
		}},
		{"neither-isolation-identity", func(r *HermeticReceiptV1, _ *HermeticAdmissionRequest) {
			r.Isolation.ContainerIdentity = ""
			resignStatic(r, signer)
		}},
		{"invalid-source-digest", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) {
			r.SourceCopyDigest = "sha256:UPPER"
			req.SourceCopyDigest = r.SourceCopyDigest
			resignStatic(r, signer)
		}},
		{"signed-host-pid-sharing", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) {
			r.HostPIDSharing = true
			req.HostPIDSharing = true
			resignStatic(r, signer)
		}},
		{"signed-host-user-namespace-sharing", func(r *HermeticReceiptV1, req *HermeticAdmissionRequest) {
			r.HostUserNamespaceSharing = true
			req.HostUserNamespaceSharing = true
			resignStatic(r, signer)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := cloneStaticReceipt(base)
			request := req
			tc.mutate(&r, &request)
			fake := newStaticReplayFake()
			called := 0
			if err := AdmitBeforeFixtureConstruction(context.Background(), r, request, verifier, fake, now, func() error { called++; return nil }); err == nil {
				t.Fatal("adversarial receipt was admitted")
			}
			if called != 0 {
				t.Fatalf("callback calls = %d, want 0", called)
			}
		})
	}

	t.Run("exact-duplicate-replay", func(t *testing.T) {
		fake := newStaticReplayFake()
		called := 0
		construct := func() error { called++; return nil }
		if err := AdmitBeforeFixtureConstruction(context.Background(), base, req, verifier, fake, now, construct); err != nil {
			t.Fatal(err)
		}
		if err := AdmitBeforeFixtureConstruction(context.Background(), base, req, verifier, fake, now, construct); err == nil {
			t.Fatal("duplicate replay was admitted")
		}
		if called != 1 {
			t.Fatalf("callback calls = %d, want 1", called)
		}
	})

	t.Run("same-nonce-conflicting-payload", func(t *testing.T) {
		fake := newStaticReplayFake()
		conflicting := cloneStaticReceipt(base)
		conflicting.Task = "FAC-198-conflict"
		conflictingRequest := req
		conflictingRequest.Task = conflicting.Task
		resignStatic(&conflicting, signer)
		called := 0
		construct := func() error { called++; return nil }
		if err := AdmitBeforeFixtureConstruction(context.Background(), base, req, verifier, fake, now, construct); err != nil {
			t.Fatal(err)
		}
		if err := AdmitBeforeFixtureConstruction(context.Background(), conflicting, conflictingRequest, verifier, fake, now, construct); err == nil {
			t.Fatal("same-nonce conflicting payload was admitted")
		}
		if called != 1 {
			t.Fatalf("callback calls = %d, want 1", called)
		}
	})

	t.Run("replay-store-failure", func(t *testing.T) {
		called := 0
		fake := staticReplayFailure{}
		if err := AdmitBeforeFixtureConstruction(context.Background(), base, req, verifier, fake, now, func() error { called++; return nil }); err == nil {
			t.Fatal("replay-store failure was admitted")
		}
		if called != 0 {
			t.Fatalf("callback calls = %d, want 0", called)
		}
	})

	t.Run("mismatched-trusted-key", func(t *testing.T) {
		otherSeed := sha256.Sum256([]byte("different launcher authority"))
		otherPrivate := ed25519.NewKeyFromSeed(otherSeed[:])
		otherVerifier, err := NewTrustedReceiptVerifier(otherPrivate.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		called := 0
		if err := AdmitBeforeFixtureConstruction(context.Background(), base, req, otherVerifier, newStaticReplayFake(), now, func() error { called++; return nil }); err == nil {
			t.Fatal("receipt was admitted with a mismatched trusted verifier")
		}
		if called != 0 {
			t.Fatalf("callback calls = %d, want 0", called)
		}
	})

	t.Run("pre-canceled-consume-creates-no-file", func(t *testing.T) {
		root := t.TempDir()
		store, err := NewFileReplayAuthority(root, verifier)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := store.ConsumeOnce(ctx, ReplayToken{Generation: base.Generation, Nonce: base.Nonce, Payload: base.PayloadDigest})
		if err == nil || result != ReplayPersistenceFailure {
			t.Fatalf("result=%v err=%v, want canceled persistence failure", result, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("pre-canceled consume created %d entries", len(entries))
		}
	})

	t.Run("authority-identity-is-fixed-by-public-key", func(t *testing.T) {
		sameKey, err := NewTrustedReceiptVerifier(append(ed25519.PublicKey(nil), verifier.publicKey...))
		if err != nil {
			t.Fatal(err)
		}
		if sameKey.authorityKeyID() != verifier.authorityKeyID() {
			t.Fatal("same public key produced different authority identities")
		}
		otherSeed := sha256.Sum256([]byte("authority identity second key"))
		otherPrivate := ed25519.NewKeyFromSeed(otherSeed[:])
		otherKey, err := NewTrustedReceiptVerifier(otherPrivate.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		if otherKey.authorityKeyID() == verifier.authorityKeyID() {
			t.Fatal("different public keys shared an authority identity")
		}
	})

	t.Run("file-replay-cross-instance", func(t *testing.T) {
		root := t.TempDir()
		first, err := NewFileReplayAuthority(root, verifier)
		if err != nil {
			t.Fatal(err)
		}
		second, err := NewFileReplayAuthority(root, verifier)
		if err != nil {
			t.Fatal(err)
		}
		token := ReplayToken{Generation: base.Generation, Nonce: base.Nonce, Payload: base.PayloadDigest}
		if result, err := first.ConsumeOnce(context.Background(), token); err != nil || result != ReplayFresh {
			t.Fatalf("first result=%v err=%v, want fresh", result, err)
		}
		if result, err := second.ConsumeOnce(context.Background(), token); err != nil || result != ReplayDuplicate {
			t.Fatalf("second result=%v err=%v, want duplicate", result, err)
		}
	})

	t.Run("file-replay-mismatched-and-corrupt", func(t *testing.T) {
		root := t.TempDir()
		store, err := NewFileReplayAuthority(root, verifier)
		if err != nil {
			t.Fatal(err)
		}
		mismatch := ReplayToken{Generation: base.Generation, Nonce: base.Nonce, Payload: "sha256:" + strings.Repeat("f", 64)}
		path := filepath.Join(root, replayFilename(verifier.authorityKeyID(), mismatch))
		if err := os.WriteFile(path, []byte(`{"version":1,"authority":"launcher-key-1","generation":"generation-1","nonce":"nonce-1","payload":"sha256:`+strings.Repeat("e", 64)+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if result, err := store.ConsumeOnce(context.Background(), mismatch); err == nil || result != ReplayConflict {
			t.Fatalf("mismatch result=%v err=%v, want conflict", result, err)
		}
		corrupt := ReplayToken{Generation: "generation-corrupt", Nonce: "nonce-corrupt", Payload: base.PayloadDigest}
		corruptPath := filepath.Join(root, replayFilename(verifier.authorityKeyID(), corrupt))
		if err := os.WriteFile(corruptPath, []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if result, err := store.ConsumeOnce(context.Background(), corrupt); err == nil || result != ReplayPersistenceFailure {
			t.Fatalf("corrupt result=%v err=%v, want persistence failure", result, err)
		}
	})
}

type staticReplayFake struct{ seen map[string]string }

func newStaticReplayFake() *staticReplayFake { return &staticReplayFake{seen: make(map[string]string)} }

func (f *staticReplayFake) ConsumeOnce(_ context.Context, token ReplayToken) (ReplayResult, error) {
	key := token.Generation + "\x00" + token.Nonce
	if prior, ok := f.seen[key]; ok {
		if prior == token.Payload {
			return ReplayDuplicate, nil
		}
		return ReplayConflict, nil
	}
	f.seen[key] = token.Payload
	return ReplayFresh, nil
}

type staticReplayFailure struct{}

func (staticReplayFailure) ConsumeOnce(context.Context, ReplayToken) (ReplayResult, error) {
	return ReplayPersistenceFailure, errors.New("durable store unavailable")
}

func staticHermeticFixture() (HermeticReceiptV1, HermeticAdmissionRequest, *TrustedReceiptVerifier, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("FAC-198 static launcher authority"))
	private := ed25519.NewKeyFromSeed(seed[:])
	verifier, _ := NewTrustedReceiptVerifier(private.Public().(ed25519.PublicKey))
	request := HermeticAdmissionRequest{
		Repository: "github.com/Kampe/Herdforge", Task: "FAC-198", CandidateSHA: strings.Repeat("a", 40),
		Argv:                 []string{"./verifier.test", "-test.run=TestStatic"},
		Isolation:            IsolationBinding{Kind: IsolationContainer, ContainerIdentity: "container:fixture"},
		PIDNamespaceIdentity: "pidns:fixture", UserNamespaceIdentity: "userns:fixture", UID: 1000, GID: 1000,
		NetworkMode: "none", MountPolicy: "immutable-copy-no-host-bind", SourceCopyDigest: "sha256:" + strings.Repeat("1", 64),
		Generation: "generation-1", Nonce: "nonce-1",
	}
	request.ArgvDigest = digestArgv(request.Argv)
	receipt := HermeticReceiptV1{
		Version: HermeticReceiptVersion, Repository: request.Repository, Task: request.Task, CandidateSHA: request.CandidateSHA,
		Argv: append([]string(nil), request.Argv...), ArgvDigest: request.ArgvDigest, Isolation: request.Isolation,
		PIDNamespaceIdentity: request.PIDNamespaceIdentity, UserNamespaceIdentity: request.UserNamespaceIdentity, UID: request.UID, GID: request.GID,
		NetworkMode: request.NetworkMode, MountPolicy: request.MountPolicy, SourceCopyDigest: request.SourceCopyDigest,
		StartedAt: time.Date(2026, 8, 4, 11, 55, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 4, 12, 5, 0, 0, time.UTC),
		Generation: request.Generation, Nonce: request.Nonce,
	}
	resignStatic(&receipt, private)
	return receipt, request, verifier, private
}

func resignStatic(receipt *HermeticReceiptV1, signer ed25519.PrivateKey) {
	receipt.PayloadDigest = payloadDigest(*receipt)
	receipt.Signature = ed25519.Sign(signer, signedPayload(*receipt))
}

func cloneStaticReceipt(receipt HermeticReceiptV1) HermeticReceiptV1 {
	receipt.Argv = append([]string(nil), receipt.Argv...)
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	return receipt
}
