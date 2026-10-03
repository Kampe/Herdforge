package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func runReviewAbort() error {
	fs := flag.NewFlagSet("review-abort", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	manifestPath := fs.String("manifest", "", "Exact review retirement/launch manifest JSON")
	session := fs.String("session", "", "Exact reviewer session id")
	reason := fs.String("reason", "", "Abort reason (for example quota-exhausted)")
	dryRun := fs.Bool("dry-run", true, "Report the abort without writing the ledger (default)")
	act := fs.Bool("act", false, "Append the coordinator-abort event")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *act {
		*dryRun = false
	}
	path := strings.TrimSpace(*manifestPath)
	sess := strings.TrimSpace(*session)
	why := strings.TrimSpace(*reason)
	if path == "" || sess == "" || why == "" {
		return fmt.Errorf("usage: herd review-abort --manifest FILE --session ID --reason TEXT [--dry-run|--act]")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m herdr.ReviewRetirementManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if err := herdr.ValidateReviewRetirementManifest(m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.SessionID != sess {
		return fmt.Errorf("session mismatch: manifest %s vs --session %s", m.SessionID, sess)
	}
	if herdr.IsAvailable() {
		agents, listErr := herdr.AgentList()
		if listErr != nil {
			return fmt.Errorf("live reviewer identity: %w", listErr)
		}
		if err := herdr.ReviewAbortLiveConflict(agents, m.Reviewer, m.PaneID, m.TabID, sess); err != nil {
			return err
		}
	}
	root, err := filepath.Abs(".")
	if err != nil {
		return err
	}
	ledgerPath := reviewLedgerPath()
	if !filepath.IsAbs(ledgerPath) {
		ledgerPath = filepath.Join(root, ledgerPath)
	}
	var ledger *reviewledger.Ledger
	if *dryRun {
		ledger, err = reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	} else {
		ledger, err = reviewledger.NewReviewLedger(root, ledgerPath)
	}
	if err != nil {
		return err
	}
	opts := reviewledger.AbortOpts{
		SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, SessionID: sess,
		Pane: m.PaneID, Task: m.TaskRef, Reason: why, Artifact: path,
	}
	if *dryRun {
		if err := ledger.CheckCoordinatorAbort(opts); err != nil {
			return err
		}
		fmt.Printf("review-abort dry-run: reviewer=%s sha=%s lease=%s session=%s reason=%s\n", m.Reviewer, m.CandidateSHA, m.Nonce, sess, why)
		return nil
	}
	if err := ledger.CoordinatorAbort(opts); err != nil {
		return err
	}
	fmt.Printf("review-abort recorded coordinator-abort for %s session %s (not a reviewer verdict)\n", m.Reviewer, sess)
	return nil
}
