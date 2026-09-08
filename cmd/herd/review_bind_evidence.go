package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

type reviewBindEvidenceArgs struct {
	Task      string
	Candidate string
	Receipt   string
}

func parseReviewBindEvidenceArgs(args []string) (reviewBindEvidenceArgs, error) {
	var out reviewBindEvidenceArgs
	var positional []string
	usage := fmt.Errorf("usage: herd review-bind-evidence <REF> --candidate <sha> --receipt <digest>")
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, next, ok, err := takeCLIFlag(args, i, "--candidate"); ok {
			if err != nil {
				return out, usage
			}
			out.Candidate = v
			i = next
			continue
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--receipt"); ok {
			if err != nil {
				return out, usage
			}
			out.Receipt = v
			i = next
			continue
		}
		switch {
		case a == "--sweep" || a == "--corpus" || strings.HasPrefix(a, "--sweep=") || strings.HasPrefix(a, "--corpus="):
			return out, fmt.Errorf("corpus mode is refused; review-bind-evidence is explicit-ref only")
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf("unknown flag %s", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		return out, usage
	}
	out.Task = strings.TrimSpace(positional[0])
	if out.Task == "" || out.Candidate == "" || out.Receipt == "" {
		return out, usage
	}
	return out, nil
}

func runReviewBindEvidence() error {
	parsed, err := parseReviewBindEvidenceArgs(os.Args[2:])
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), cwd)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	store, err := verifier.NewFileReceiptStore(filepath.Join(root, defaultReceiptDir))
	if err != nil {
		return err
	}
	receipt, err := store.Load(context.Background(), parsed.Receipt)
	if err != nil {
		return fmt.Errorf("load receipt %s: %w", parsed.Receipt, err)
	}
	ledgerPath := reviewLedgerPath()
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		return err
	}
	return ledger.BindEvidence(parsed.Task, parsed.Candidate, parsed.Receipt, receipt)
}
