package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/integration"
)

// runIntegrate drives one candidate's integration lifecycle, one step per
// invocation, against a durable record.
//
// FAC-710: FAC-709 gave the lifecycle an ORDER and the refusals that make
// cleanup safe. It had no driver and no durability, so nothing could resume a
// transaction and nothing enforced the order in practice.
//
// One step per invocation is deliberate. A driver that runs the whole lifecycle
// unattended would perform a merge and a destructive cleanup from a single
// command, and this session has repeatedly shown what a confident-but-wrong
// automated step costs. The coordinator advances the transaction; the record
// says where it is.
func runIntegrate(args []string) error {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	candidate := fs.String("candidate", "", "exact candidate sha (required)")
	step := fs.String("step", "", "step to record as complete")
	evidence := fs.String("evidence", "", "what proves this step happened")
	status := fs.Bool("status", false, "report where this transaction stands")
	asJSON := fs.Bool("json", false, "structured output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*candidate) == "" {
		return fmt.Errorf("--candidate is required: an integration keyed on nothing cannot prove which content it landed")
	}

	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	tx, err := integration.Load(root, *candidate)
	if err != nil {
		return err
	}

	if *status || strings.TrimSpace(*step) == "" {
		next, more := tx.Next()
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(struct {
				*integration.Transaction
				Next     string `json:"next_step,omitempty"`
				Complete bool   `json:"complete"`
			}{tx, string(next), !more})
		}
		if !more {
			fmt.Printf("integration %s: COMPLETE (%d/%d steps)\n", shortSHA12(tx.Candidate), len(tx.Done), len(integration.Order))
			return nil
		}
		fmt.Printf("integration %s: %d/%d steps done, next=%s\n",
			shortSHA12(tx.Candidate), len(tx.Done), len(integration.Order), next)
		for _, r := range tx.Done {
			fmt.Printf("  %-22s %s\n", r.Step, r.Evidence)
		}
		return nil
	}

	// Record, then persist. Persisting BEFORE the next step can begin is what
	// makes a crash diagnosable rather than ambiguous.
	if err := tx.Complete(integration.Step(strings.TrimSpace(*step)), *evidence); err != nil {
		return err
	}
	if err := integration.Save(root, tx); err != nil {
		return fmt.Errorf("step recorded in memory but NOT persisted, so it cannot be resumed: %w", err)
	}
	next, more := tx.Next()
	if !more {
		fmt.Printf("integration %s: COMPLETE\n", shortSHA12(tx.Candidate))
		return nil
	}
	fmt.Printf("integration %s: recorded %s; next=%s\n", shortSHA12(tx.Candidate), *step, next)
	return nil
}
