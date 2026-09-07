package reviewack

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestArtifactAcknowledgmentsPreserveReassessmentAndLegacy(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	original := Ack{SHA: sha, Reviewer: "review-one", ArtifactDigest: ArtifactDigest([]byte("old")), LaunchIdentity: "review-one"}
	if err := Emit(root, original); err != nil {
		t.Fatal(err)
	}
	legacy, err := os.ReadFile(Path(root, sha, original.Reviewer))
	if err != nil {
		t.Fatal(err)
	}
	newer := original
	newer.ArtifactDigest = ArtifactDigest([]byte("reassessment"))
	if err := EmitArtifact(root, newer); err != nil {
		t.Fatal(err)
	}
	if got := Consume(root, sha, newer.Reviewer, newer.ArtifactDigest, newer.LaunchIdentity); !got.OK {
		t.Fatalf("new admitted artifact refused: %+v", got)
	}
	if got := Consume(root, sha, original.Reviewer, original.ArtifactDigest, original.LaunchIdentity); !got.OK {
		t.Fatalf("legacy artifact lost: %+v", got)
	}
	after, err := os.ReadFile(Path(root, sha, original.Reviewer))
	if err != nil || string(after) != string(legacy) {
		t.Fatal("legacy acknowledgment overwritten", err)
	}
	if got := Consume(root, sha, newer.Reviewer, newer.ArtifactDigest, "another-session"); got.OK {
		t.Fatal("wrong identity accepted")
	}
	if got := Consume(root, sha, newer.Reviewer, ArtifactDigest([]byte("never admitted")), newer.LaunchIdentity); got.OK {
		t.Fatal("unknown artifact accepted")
	}
}

func TestArtifactAckConcurrentPublicationCannotChangeIdentity(t *testing.T) {
	root := t.TempDir()
	ack := Ack{SHA: strings.Repeat("b", 40), Reviewer: "review-one", ArtifactDigest: ArtifactDigest([]byte("same")), LaunchIdentity: "review-one"}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := EmitArtifact(root, ack); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	changed := ack
	changed.LaunchIdentity = "different"
	if err := EmitArtifact(root, changed); err == nil {
		t.Fatal("conflicting publisher replaced identity")
	}
	if got := Consume(root, ack.SHA, ack.Reviewer, ack.ArtifactDigest, ack.LaunchIdentity); !got.OK {
		t.Fatalf("valid ack lost: %+v", got)
	}
}

func TestArtifactAckFullIdentityAvoidsLegacyNameCollisions(t *testing.T) {
	root := t.TempDir()
	a := Ack{SHA: strings.Repeat("a", 40), Reviewer: "review/A", ArtifactDigest: ArtifactDigest([]byte("body"))}
	b := a
	b.SHA = strings.Repeat("a", 12) + strings.Repeat("b", 28)
	b.Reviewer = "review-A"
	if FileName(a.SHA, a.Reviewer) != FileName(b.SHA, b.Reviewer) {
		t.Fatal("fixture must collide in legacy key")
	}
	for _, ack := range []Ack{a, b} {
		if err := EmitArtifact(root, ack); err != nil {
			t.Fatal(err)
		}
		if got := Consume(root, ack.SHA, ack.Reviewer, ack.ArtifactDigest, ack.Reviewer); !got.OK {
			t.Fatalf("binding lost: %+v", got)
		}
	}
}
