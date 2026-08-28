package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-132: closing a card is an assertion about reality, so the CLI carries
// exactly two authorities — the completion receipt the integration produced,
// or an explicit, policy-limited, attributable manual override. --force is
// gone: it recorded nothing about who forced what, or why.

// overrideFlags holds the manual-override quartet. All four are required
// together; any subset is refused by pkg/sync.
type overrideFlags struct {
	policy   *string
	actor    *string
	reason   *string
	evidence *string
	force    *bool
}

func registerOverrideFlags(fs *flag.FlagSet) overrideFlags {
	return overrideFlags{
		policy:   fs.String("override-policy", "", "Manual-override policy (one of: "+overridePolicyList()+")"),
		actor:    fs.String("override-actor", "", "Who is taking responsibility for the manual override"),
		reason:   fs.String("override-reason", "", "Why this card is being closed without a receipt"),
		evidence: fs.String("override-evidence", "", "What the actor looked at (sha, URL, ticket)"),
		force:    fs.Bool("force", false, "REMOVED: use the --override-* flags"),
	}
}

func overridePolicyList() string {
	out := ""
	for _, p := range hsync.SortedOverridePolicies() {
		if out != "" {
			out += ", "
		}
		out += p
	}
	return out
}

// request returns the override request, or nil ONLY when no override flag was
// given at all. --force is refused here; a partial quartet is deliberately
// still assembled and carried to authorizeOverride, which names the missing
// field. What must never happen is a half-filled override collapsing to nil
// and silently degrading into "no authority at all" — that would surface as
// the generic no-receipt refusal instead of "manual override requires actor".
func (f overrideFlags) request() (*hsync.OverrideRequest, error) {
	if f.force != nil && *f.force {
		return nil, fmt.Errorf("--force is no longer accepted: it closed cards without recording who forced what. " +
			"Use --override-policy/--override-actor/--override-reason/--override-evidence")
	}
	if *f.policy == "" && *f.actor == "" && *f.reason == "" && *f.evidence == "" {
		return nil, nil
	}
	return &hsync.OverrideRequest{
		Policy: *f.policy, Actor: *f.actor, Reason: *f.reason, Evidence: *f.evidence,
	}, nil
}

// loadDoneReceipt resolves the completion receipt for ref. An explicit path
// that does not exist is an error; a missing default path just means "no
// receipt", which BoardDone refuses with its own message.
func loadDoneReceipt(repoDir, ref, explicitPath string) (*hsync.CompletionReceipt, error) {
	path := explicitPath
	if path == "" {
		path = hsync.ReceiptPath(repoDir, ref)
		if _, err := os.Stat(path); err != nil {
			return nil, nil
		}
	}
	return hsync.LoadReceipt(path)
}

// openLifecycleAuthority opens the canonical durable state store read-only for
// BoardDone's use. The caller must call the returned closer.
func openLifecycleAuthority(repoDir string) (hsync.LifecycleAuthority, func(), error) {
	store, err := lifecycle.NewEventStore(lifecycle.CanonicalStatePath(repoDir))
	if err != nil {
		return nil, func() {}, fmt.Errorf("open lifecycle state: %w", err)
	}
	return store, func() { _ = store.Close() }, nil
}

// buildDoneRequest assembles the request for one card. It opens the lifecycle
// store only when there is a receipt to check against it.
func buildDoneRequest(repoDir, projectID, ref, receiptPath, acceptanceEvidence string, override *hsync.OverrideRequest) (hsync.DoneRequest, func(), error) {
	req := hsync.DoneRequest{RepoDir: repoDir, ProjectID: projectID, Ref: ref, Override: override}
	req.AcceptanceEvidence = acceptanceEvidence
	closer := func() {}
	if override != nil {
		return req, closer, nil
	}
	receipt, err := loadDoneReceipt(repoDir, ref, receiptPath)
	if err != nil {
		return req, closer, err
	}
	if receipt == nil {
		return req, closer, nil
	}
	req.Receipt = receipt
	if strings.TrimSpace(req.AcceptanceEvidence) == "" {
		req.AcceptanceEvidence = receipt.AcceptanceEvidence
	}
	if receipt.ProvenanceMode == hsync.ProvenanceReduced {
		return req, closer, nil
	}
	authority, closer, err := openLifecycleAuthority(repoDir)
	if err != nil {
		return req, func() {}, err
	}
	req.Lifecycle = authority
	return req, closer, nil
}

// finishBoardDone reports the outcome of a successful close and ALWAYS
// surrenders the ticket's scope claim.
//
// The release is unconditional on purpose. A closed ticket that keeps its
// claim stays Active forever and blocks any later task overlapping it —
// FAC-174 was merged and board-closed yet still held pkg/verifier, which
// rejected FAC-198 with scope_overlap and needed a manual release to clear.
// Re-running `herd board-done` is the documented recovery for that, and for a
// crash between the recorded close and the release. FAC-132's exactly-once
// short-circuit makes redelivery skip the board write — it must NOT also skip
// this, or the recovery becomes a permanent no-op. release is injected so
// "always" is a testable property rather than a comment.
func finishBoardDone(out io.Writer, res *hsync.DoneResult, release func(string)) {
	if res.Idempotent {
		fmt.Fprintf(out, "herd board-done: %s was already closed by receipt %s; no board change this run\n",
			res.Ref, res.ReceiptDigest)
	} else {
		fmt.Fprintf(out, "herd board-done: %s proof: %s\n", res.Ref, res.Proof)
		fmt.Fprintf(out, "herd board-done: %s is done (verified by read-back)\n", res.Ref)
	}
	release(res.Ref)
}

// runBoardAudit reports Done cards that no completion receipt closed. It never
// writes to the board: the historical damage was a machine deciding a human's
// acceptance criteria, and auto-reopening would repeat it in reverse.
//
// Exit codes: 0 clean, 3 suspicious cards found, 1 operational error.
func runBoardAudit() {
	fs := flag.NewFlagSet("board-audit", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Emit findings as JSON")
	// FAC-553: 528 undifferentiated findings is not a control. The baseline
	// separates inherited damage from a fresh bypass so the actionable count
	// starts at zero and any increase is a real regression.
	newOnly := fs.Bool("new-only", false, "Report only violations that are not in the accepted baseline")
	acceptBaseline := fs.Bool("accept-baseline", false, "Record the current unclosed set as inherited-historical (requires --actor)")
	actor := fs.String("actor", "", "Who is accepting the baseline, for attribution")
	fs.Parse(os.Args[2:])

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "task provider: %v\n", tpErr)
		os.Exit(1)
	}

	findings, err := hsync.AuditDone(context.Background(), tp, ".", cfg.TaskProvider.ProjectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-audit: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(findings); err != nil {
			fmt.Fprintf(os.Stderr, "herd board-audit: %v\n", err)
			os.Exit(1)
		}
		if len(findings) > 0 {
			os.Exit(3)
		}
		return
	}

	baseline, baseErr := hsync.ReadAuditBaseline(".")
	if baseErr != nil {
		// Fail closed: a corrupt baseline must not degrade to "all historical".
		fmt.Fprintf(os.Stderr, "herd board-audit: %v\n", baseErr)
		os.Exit(1)
	}

	if *acceptBaseline {
		ids := make([]string, 0, len(findings))
		for _, f := range findings {
			if f.Kind != hsync.AuditOverride {
				ids = append(ids, f.TaskID)
			}
		}
		written, werr := hsync.WriteAuditBaseline(".", *actor, ids)
		if werr != nil {
			fmt.Fprintf(os.Stderr, "herd board-audit: %v\n", werr)
			os.Exit(1)
		}
		fmt.Printf("herd board-audit: accepted %d inherited finding(s) as baseline (actor=%s at %s)\n",
			len(written.TaskIDs), written.Actor, written.CapturedAt)
		fmt.Println("  This is NOT evidence of completion. New bypasses now report as violations.")
		return
	}

	violations, historical := hsync.PartitionFindings(findings, baseline)

	for _, f := range violations {
		fmt.Printf("%-16s [%s] %s\n  %s\n", f.Kind, f.Ref, f.Title, f.Detail)
	}
	if !*newOnly {
		for _, f := range historical {
			fmt.Printf("%-16s [%s] %s (inherited)\n  %s\n", f.Kind, f.Ref, f.Title, f.Detail)
		}
	}
	fmt.Printf("\nherd board-audit: %d new violation(s), %d inherited\n", len(violations), len(historical))
	if baseline == nil {
		fmt.Println("  No baseline accepted yet, so every finding counts as new.")
		fmt.Println("  Accept the inherited set once: herd board-audit --accept-baseline --actor <who>")
	}
	// Exit non-zero only on NEW violations. A permanently red audit trains
	// operators to ignore it, which is how 528 findings accumulated.
	if len(violations) > 0 {
		os.Exit(3)
	}
}
