package reviewack

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ArtifactPath names an immutable acknowledgment by the full artifact binding.
// Hashing the tuple avoids shortened-SHA and sanitized-reviewer collisions.
func ArtifactPath(root, sha, reviewer, digest string) string {
	binding, _ := json.Marshal([]string{strings.ToLower(sha), reviewer, strings.ToLower(digest)})
	return filepath.Join(root, DirRel, "artifacts", ArtifactDigest(binding)+".json")
}

// EmitArtifact is called only after exact ledger admission is established.
// Separate artifacts from an authenticated reassessment retain separate acks.
// Legacy Emit and its ambiguity protection remain available to old callers.
func EmitArtifact(root string, ack Ack) error {
	ack.SHA = strings.ToLower(strings.TrimSpace(ack.SHA))
	ack.ArtifactDigest = strings.ToLower(strings.TrimSpace(ack.ArtifactDigest))
	ack.Reviewer = strings.TrimSpace(ack.Reviewer)
	ack.LaunchIdentity = strings.TrimSpace(ack.LaunchIdentity)
	if ack.LaunchIdentity == "" {
		ack.LaunchIdentity = ack.Reviewer
	}
	sha, e1 := hex.DecodeString(ack.SHA)
	digest, e2 := hex.DecodeString(ack.ArtifactDigest)
	if strings.TrimSpace(root) == "" || e1 != nil || len(sha) != 20 || e2 != nil || len(digest) != 32 || ack.Reviewer == "" {
		return fmt.Errorf("reviewack: exact root, sha, reviewer and artifact digest required")
	}
	ack.SchemaVersion = 2
	if ack.AdmittedAt == "" {
		ack.AdmittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return publishImmutable(ArtifactPath(root, ack.SHA, ack.Reviewer, ack.ArtifactDigest), ack)
}

func publishImmutable(path string, ack Ack) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Link publishes complete bytes without overwriting a concurrent publisher.
	if err = os.Link(tmp.Name(), path); err != nil {
		if !os.IsExist(err) {
			return err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var prior Ack
		if json.Unmarshal(existing, &prior) != nil || prior.SHA != ack.SHA || prior.Reviewer != ack.Reviewer || prior.ArtifactDigest != ack.ArtifactDigest || prior.LaunchIdentity != ack.LaunchIdentity {
			return fmt.Errorf("%w: immutable acknowledgment binding differs", ErrAmbiguous)
		}
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func artifactConsumedPath(root, sha, reviewer, digest string) string {
	return filepath.Join(root, ConsumedDirRel, "artifacts", filepath.Base(ArtifactPath(root, sha, reviewer, digest)))
}
