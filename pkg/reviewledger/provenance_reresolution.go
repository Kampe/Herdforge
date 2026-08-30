package reviewledger

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
)

// GateReceiptResolvedProvenance marks an append-only launch record whose
// historical unrecorded builder family was recovered from a reaching receipt.
const GateReceiptResolvedProvenance = "receipt-resolved-provenance"

// LaunchProvenanceResolutionOpts identifies one exact historical launch row
// and the receipt evidence permitted to supersede its unrecorded family.
type LaunchProvenanceResolutionOpts struct {
	SHA         string
	Reviewer    string
	ReceiptPath string
	CommitTime  time.Time
	Reaches     func(branch string) bool
	Apply       bool
}

// LaunchProvenanceResolution exposes only safe decision fields. Receipt bodies
// and secret-bearing ledger fields are never copied into command diagnostics.
type LaunchProvenanceResolution struct {
	SHA              string
	Reviewer         string
	PreviousFamily   string
	ResolvedFamily   string
	Gate             string
	WouldAppend      bool
	Appended         bool
	Idempotent       bool
	ReadbackVerified bool
}

// ReResolveLaunchProvenance appends receipt-backed provenance for an exact
// historical provenance-unrecorded record. It never edits or backfills an
// existing row. Apply serializes receipt resolution, ledger recheck, append,
// and exact readback under one cross-process mutation lock.
func (l *Ledger) ReResolveLaunchProvenance(opts LaunchProvenanceResolutionOpts) (LaunchProvenanceResolution, error) {
	var out LaunchProvenanceResolution
	if l == nil {
		return out, fmt.Errorf("provenance re-resolution refused: admission_condition=ledger observed configured=false")
	}
	sha := l.NormalizeSHA(strings.TrimSpace(opts.SHA))
	reviewer := strings.TrimSpace(opts.Reviewer)
	out.SHA, out.Reviewer = sha, reviewer
	if sha == "" || reviewer == "" {
		return out, fmt.Errorf("provenance re-resolution refused: admission_condition=record_identity observed sha=%q reviewer=%q", sha, reviewer)
	}
	if strings.TrimSpace(opts.ReceiptPath) == "" {
		return out, fmt.Errorf("provenance re-resolution refused: admission_condition=launch_receipt observed receipt_path_set=false")
	}
	if opts.CommitTime.IsZero() {
		return out, fmt.Errorf("provenance re-resolution refused: admission_condition=commit_time observed commit_time_set=false")
	}
	if opts.Reaches == nil {
		return out, fmt.Errorf("provenance re-resolution refused: admission_condition=reachability observed reachability_probe_configured=false")
	}

	transaction := func() error {
		family, err := resolveReceiptFamily(opts.ReceiptPath, sha, opts.CommitTime, opts.Reaches)
		if err != nil {
			return err
		}
		out.ResolvedFamily = family
		if !FamilyAllowlist[family] {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=builder_family observed builder_family=%q allowlisted=false", family)
		}

		rows, err := readRows(l.Path)
		if err != nil {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=review_ledger observed ledger_readable=false: %w", err)
		}
		var prior *LedgerRow
		for i := range rows {
			if rows[i].Event == string(EventRecord) && rows[i].SHA == sha && strings.TrimSpace(rows[i].Reviewer) == reviewer {
				row := rows[i]
				prior = &row
			}
		}
		if prior == nil {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=launch_record observed launch_record_present=false sha=%q reviewer=%q", sha, reviewer)
		}
		out.PreviousFamily = strings.TrimSpace(prior.BuilderFamily)
		if FamilyAllowlist[out.PreviousFamily] {
			if out.PreviousFamily != family {
				return fmt.Errorf("provenance re-resolution refused: admission_condition=builder_family_conflict observed recorded_builder_family=%q receipt_builder_family=%q", out.PreviousFamily, family)
			}
			out.Gate = prior.Gate
			out.Idempotent = true
			out.ReadbackVerified = true
			return nil
		}
		if out.PreviousFamily != FamilyUnrecorded || prior.Gate != GateProvenanceUnrecorded {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=historical_provenance observed builder_family=%q gate=%q required_builder_family=%q required_gate=%q",
				out.PreviousFamily, prior.Gate, FamilyUnrecorded, GateProvenanceUnrecorded)
		}

		out.WouldAppend = true
		out.Gate = GateReceiptResolvedProvenance
		if !opts.Apply {
			return nil
		}

		next := *prior
		next.BuilderFamily = family
		next.Gate = GateReceiptResolvedProvenance
		if err := l.appendRow(l.Path, &next); err != nil {
			return fmt.Errorf("append receipt-resolved provenance: %w", err)
		}
		backRows, err := readRows(l.Path)
		if err != nil {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=readback observed readback=false: %w", err)
		}
		var back *LedgerRow
		for i := len(backRows) - 1; i >= 0; i-- {
			if backRows[i].Event == string(EventRecord) && backRows[i].SHA == sha && strings.TrimSpace(backRows[i].Reviewer) == reviewer {
				row := backRows[i]
				back = &row
				break
			}
		}
		if back == nil || *back != next {
			return fmt.Errorf("provenance re-resolution refused: admission_condition=readback observed readback=false exact_row_match=false")
		}
		out.Appended = true
		out.ReadbackVerified = true
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !opts.Apply {
		return out, transaction()
	}
	if err := withLedgerMutationLock(l.Path, transaction); err != nil {
		return out, err
	}
	return out, nil
}

func resolveReceiptFamily(path, sha string, commitTime time.Time, reaches func(branch string) bool) (string, error) {
	receipts, err := launch.ReadReceipts(path)
	if err != nil {
		return "", fmt.Errorf("provenance re-resolution refused: admission_condition=launch_receipt observed receipt_log_readable=false")
	}
	var latest time.Time
	families := make(map[string]struct{})
	qualifying := 0
	for _, receipt := range receipts {
		if !receipt.Accepted {
			continue
		}
		branch := strings.TrimSpace(receipt.Branch)
		family := strings.TrimSpace(receipt.BuilderFamily)
		if branch == "" || family == "" || receipt.CreatedAt.IsZero() || receipt.CreatedAt.After(commitTime) {
			continue
		}
		if !reaches(branch) {
			continue
		}
		if latest.IsZero() || receipt.CreatedAt.After(latest) {
			latest = receipt.CreatedAt
			families = map[string]struct{}{family: {}}
			qualifying = 1
			continue
		}
		if receipt.CreatedAt.Equal(latest) {
			families[family] = struct{}{}
			qualifying++
		}
	}
	if latest.IsZero() {
		return "", fmt.Errorf("provenance re-resolution refused: admission_condition=launch_receipt observed receipt_resolution=none qualifying_receipts=0 candidate_sha=%q", sha)
	}
	if len(families) != 1 {
		observed := make([]string, 0, len(families))
		for family := range families {
			observed = append(observed, family)
		}
		sort.Strings(observed)
		return "", fmt.Errorf("provenance re-resolution refused: admission_condition=launch_receipt observed receipt_resolution=ambiguous qualifying_receipts=%d created_at=%q builder_families=%q",
			qualifying, latest.UTC().Format(time.RFC3339Nano), observed)
	}
	for family := range families {
		return family, nil
	}
	return "", fmt.Errorf("provenance re-resolution refused: admission_condition=launch_receipt observed receipt_resolution=none qualifying_receipts=0")
}

func withLedgerMutationLock(path string, fn func() error) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("provenance re-resolution refused: admission_condition=concurrency observed mutation_lock_open=false: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockErr := fmt.Errorf("provenance re-resolution refused: admission_condition=concurrency observed mutation_lock_acquired=false: %w", err)
		return errors.Join(lockErr, lock.Close())
	}
	mutationErr := fn()
	unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	closeErr := lock.Close()
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock review ledger mutation: %w", unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close review ledger mutation lock: %w", closeErr)
	}
	return errors.Join(mutationErr, unlockErr, closeErr)
}
