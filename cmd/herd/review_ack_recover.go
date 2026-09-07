package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// admittedArtifactAck checks existing admission rather than performing admission.
// This permits crash recovery after a candidate has landed (and its diff is now
// empty), without relaxing any gate on a new or changed verdict.
func admittedArtifactAck(root string, ledger reviewIngestLedger, body []byte) (reviewack.Ack, error) {
	a := reviewingest.Parse(string(body))
	row, found, err := ledger.VerdictForReviewer(a.SHA, a.Reviewer)
	if err != nil {
		return reviewack.Ack{}, err
	}
	digest := reviewack.ArtifactDigest(body)
	if !found || row.Event != string(reviewledger.EventVerdict) || row.SHA != a.SHA || row.Reviewer != a.Reviewer || row.ArtifactDigest != digest || row.Task != a.TaskRef || row.Verdict != a.Verdict {
		return reviewack.Ack{}, fmt.Errorf("ack recovery requires the exact current admitted verdict artifact")
	}
	if row.Artifact == "" || filepath.IsAbs(row.Artifact) {
		return reviewack.Ack{}, fmt.Errorf("ack recovery requires a retained repository artifact")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return reviewack.Ack{}, err
	}
	retained, err := filepath.EvalSymlinks(filepath.Join(canonical, row.Artifact))
	if err != nil {
		return reviewack.Ack{}, err
	}
	rel, err := filepath.Rel(canonical, retained)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return reviewack.Ack{}, fmt.Errorf("retained artifact escapes repository")
	}
	stored, err := os.ReadFile(retained)
	if err != nil {
		return reviewack.Ack{}, err
	}
	if !bytes.Equal(stored, body) {
		return reviewack.Ack{}, fmt.Errorf("retained artifact bytes differ from requested acknowledgment")
	}
	return reviewack.Ack{SHA: row.SHA, Reviewer: row.Reviewer, ArtifactDigest: digest, LaunchIdentity: row.Reviewer, AdmittedAt: row.Timestamp}, nil
}

func recoverReviewArtifactAck(root string, ledger reviewIngestLedger, body []byte, dryRun bool) error {
	ack, err := admittedArtifactAck(root, ledger, body)
	if err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	return reviewack.EmitArtifact(root, ack)
}
