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

func TestReadAndReadArtifact(t *testing.T) {
	root := t.TempDir()
	shaLegacy := strings.Repeat("c", 40)
	digestLegacy := ArtifactDigest([]byte("legacy-bytes"))
	ackLegacy := Ack{SHA: shaLegacy, Reviewer: "review-legacy", ArtifactDigest: digestLegacy, LaunchIdentity: "review-legacy"}
	if err := Emit(root, ackLegacy); err != nil {
		t.Fatal(err)
	}
	readLegacy, err := Read(root, shaLegacy, "review-legacy")
	if err != nil || readLegacy.ArtifactDigest != digestLegacy {
		t.Fatalf("read legacy: %v, ack=%+v", err, readLegacy)
	}

	shaArtifact := strings.Repeat("d", 40)
	digestArtifact := ArtifactDigest([]byte("artifact-bytes"))
	ackArtifact := Ack{SHA: shaArtifact, Reviewer: "review-artifact", ArtifactDigest: digestArtifact, LaunchIdentity: "review-artifact"}
	if err := EmitArtifact(root, ackArtifact); err != nil {
		t.Fatal(err)
	}
	readWithDigest, err := ReadArtifact(root, shaArtifact, "review-artifact", digestArtifact)
	if err != nil || readWithDigest.ArtifactDigest != digestArtifact {
		t.Fatalf("read with digest: %v, ack=%+v", err, readWithDigest)
	}
	readInferred, err := Read(root, shaArtifact, "review-artifact")
	if err != nil || readInferred.ArtifactDigest != digestArtifact {
		t.Fatalf("read inferred: %v, ack=%+v", err, readInferred)
	}

	// Mismatched digest fails closed
	if _, err := ReadArtifact(root, shaArtifact, "review-artifact", "wrong-digest"); err == nil {
		t.Fatal("expected error on mismatched digest")
	}

	// Missing ack fails closed
	if _, err := Read(root, strings.Repeat("e", 40), "review-missing"); err == nil {
		t.Fatal("expected error on missing ack")
	}
}
