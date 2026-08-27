package reviewack

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitConsumeIdempotentAndMismatchRetain(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	body := []byte("verdict artifact bytes\n")
	digest := ArtifactDigest(body)
	ack := Ack{SHA: sha, Reviewer: "review-cha-1", ArtifactDigest: digest, LaunchIdentity: "review-cha-1"}
	if err := Emit(root, ack); err != nil {
		t.Fatal(err)
	}
	if err := Emit(root, ack); err != nil {
		t.Fatalf("identical re-emit must be idempotent: %v", err)
	}
	// Ambiguous digest for same identity fails closed.
	bad := ack
	bad.ArtifactDigest = ArtifactDigest([]byte("other"))
	if err := Emit(root, bad); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguous error, got %v", err)
	}

	got := Consume(root, sha, "review-cha-1", digest, "review-cha-1")
	if !got.OK {
		t.Fatalf("consume: %+v", got)
	}
	if got.Layer != "ingest_ack" {
		t.Fatalf("layer=%q", got.Layer)
	}
	// Duplicate consume is OK.
	got2 := Consume(root, sha, "review-cha-1", digest, "review-cha-1")
	if !got2.OK || !strings.Contains(got2.Reason, "idempotent") {
		t.Fatalf("duplicate consume: %+v", got2)
	}
}

func TestConsumeMissingAndIdentityMismatchRetain(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("b", 40)
	digest := ArtifactDigest([]byte("x"))
	miss := Consume(root, sha, "review-cha-2", digest, "review-cha-2")
	if miss.OK || !strings.Contains(miss.Reason, "missing") {
		t.Fatalf("missing: %+v", miss)
	}
	if err := Emit(root, Ack{SHA: sha, Reviewer: "review-cha-2", ArtifactDigest: digest, LaunchIdentity: "review-cha-2"}); err != nil {
		t.Fatal(err)
	}
	wrong := Consume(root, sha, "review-cha-2", digest, "other-launch")
	if wrong.OK || !strings.Contains(wrong.Reason, "launch identity") {
		t.Fatalf("identity mismatch: %+v", wrong)
	}
	stale := Consume(root, sha, "review-cha-2", ArtifactDigest([]byte("stale")), "review-cha-2")
	if stale.OK || !strings.Contains(stale.Reason, "stale") && !strings.Contains(stale.Reason, "digest") {
		t.Fatalf("stale digest: %+v", stale)
	}
	_ = filepath.Join(root, DirRel) // ensure constants referenced
}
