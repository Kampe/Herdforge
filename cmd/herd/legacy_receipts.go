package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/candidateindex"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
)

const legacyReceiptLog = ".herd/legacy-receipts.jsonl"

type legacyReceiptTombstone struct {
	Kind       string    `json:"kind"`
	TaskRef    string    `json:"task_ref"`
	TaskID     string    `json:"task_id"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	RecordedAt time.Time `json:"recorded_at"`
}

type legacyReceiptFinding struct {
	Ref        string `json:"ref"`
	TaskID     string `json:"task_id"`
	Title      string `json:"title"`
	HasReceipt bool   `json:"has_receipt"`
	Tombstoned bool   `json:"tombstoned"`
	Reason     string `json:"reason,omitempty"`
}

func readLegacyReceiptTombstones(path string) (map[string]legacyReceiptTombstone, error) {
	out := make(map[string]legacyReceiptTombstone)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open legacy receipt log: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for line := 1; s.Scan(); line++ {
		var rec legacyReceiptTombstone
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("legacy receipt log line %d: %w", line, err)
		}
		if rec.Kind != "legacy-receipt-tombstone" || strings.TrimSpace(rec.TaskRef) == "" || strings.TrimSpace(rec.Reason) == "" {
			return nil, fmt.Errorf("legacy receipt log line %d: malformed tombstone", line)
		}
		out[strings.ToUpper(rec.TaskRef)] = rec
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read legacy receipt log: %w", err)
	}
	return out, nil
}

func appendLegacyReceiptTombstone(path string, rec legacyReceiptTombstone) error {
	if rec.Kind == "" {
		rec.Kind = "legacy-receipt-tombstone"
	}
	if strings.TrimSpace(rec.TaskRef) == "" || strings.TrimSpace(rec.Reason) == "" {
		return fmt.Errorf("legacy receipt tombstone requires task ref and reason")
	}
	if rec.RecordedAt.IsZero() {
		rec.RecordedAt = time.Now().UTC()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode legacy receipt tombstone: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create legacy receipt log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open legacy receipt log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write legacy receipt tombstone: %w", err)
	}
	return nil
}

func legacyReceiptFindings(root string, tasks []*provider.Task, tombstones map[string]legacyReceiptTombstone) ([]legacyReceiptFinding, error) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i] == nil || tasks[j] == nil {
			return tasks[i] != nil
		}
		pi, pj := candidateindex.PriorityRank(tasks[i].Priority), candidateindex.PriorityRank(tasks[j].Priority)
		if pi != pj {
			return pi > pj
		}
		return provider.CompareRefs(tasks[i].Ref, tasks[j].Ref) < 0
	})
	findings := make([]legacyReceiptFinding, 0, len(tasks))
	for _, task := range tasks {
		if task == nil || strings.TrimSpace(task.Ref) == "" {
			continue
		}
		_, receiptErr := dispatch.LoadCanonicalReceipt(root, task.Ref)
		finding := legacyReceiptFinding{Ref: task.Ref, TaskID: task.ID, Title: task.Title, HasReceipt: receiptErr == nil}
		if tombstone, ok := tombstones[strings.ToUpper(task.Ref)]; ok {
			finding.Tombstoned = true
			finding.Reason = tombstone.Reason
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

// runLegacyReceipts audits receiptless in-progress cards. Tombstoning is an
// explicit, append-only operator record; it is never accepted as merge
// authority, so FAC-145 remains fail-closed until the task is re-dispatched
// and receives a canonical launch receipt.
func runLegacyReceipts() {
	fs := flag.NewFlagSet("legacy-receipts", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output the audit as JSON")
	tombstoneRef := fs.String("tombstone", "", "Record an explicit legacy tombstone for this in-progress task ref")
	reason := fs.String("reason", "", "Required reason for --tombstone")
	fs.Parse(os.Args[2:])

	root, err := canonicalHerdRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.LoadConfig(config.PathFor(root))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: load config: %v\n", err)
		os.Exit(1)
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: task provider: %v\n", err)
		os.Exit(1)
	}
	tasks, err := tp.ListTasks(context.Background(), cfg.TaskProvider.ProjectID, "in-progress")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: list in-progress tasks: %v\n", err)
		os.Exit(1)
	}
	logPath := filepath.Join(root, legacyReceiptLog)
	tombstones, err := readLegacyReceiptTombstones(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*tombstoneRef) != "" {
		if strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "herd legacy-receipts: --reason is required with --tombstone")
			os.Exit(2)
		}
		var task *provider.Task
		for _, candidate := range tasks {
			if candidate != nil && strings.EqualFold(candidate.Ref, *tombstoneRef) {
				task = candidate
				break
			}
		}
		if task == nil {
			fmt.Fprintf(os.Stderr, "herd legacy-receipts: %s is not in-progress\n", *tombstoneRef)
			os.Exit(1)
		}
		if _, receiptErr := dispatch.LoadCanonicalReceipt(root, task.Ref); receiptErr == nil {
			fmt.Fprintf(os.Stderr, "herd legacy-receipts: %s already has a canonical receipt; refusing tombstone\n", task.Ref)
			os.Exit(1)
		}
		if existing, ok := tombstones[strings.ToUpper(task.Ref)]; ok {
			fmt.Printf("herd legacy-receipts: %s already tombstoned at %s (%s)\n", task.Ref, existing.RecordedAt.UTC().Format(time.RFC3339), existing.Reason)
		} else {
			actor, actorErr := provider.ProcessOwnerID()
			if actorErr != nil {
				fmt.Fprintf(os.Stderr, "herd legacy-receipts: actor identity: %v\n", actorErr)
				os.Exit(1)
			}
			rec := legacyReceiptTombstone{TaskRef: task.Ref, TaskID: task.ID, Reason: strings.TrimSpace(*reason), Actor: actor}
			if err := appendLegacyReceiptTombstone(logPath, rec); err != nil {
				fmt.Fprintf(os.Stderr, "herd legacy-receipts: %v\n", err)
				os.Exit(1)
			}
			tombstones[strings.ToUpper(task.Ref)] = rec
		}
	}
	findings, err := legacyReceiptFindings(root, tasks, tombstones)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd legacy-receipts: audit: %v\n", err)
		os.Exit(1)
	}
	legacy := 0
	for _, finding := range findings {
		if !finding.HasReceipt {
			legacy++
		}
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"in_progress": len(findings), "receiptless": legacy, "findings": findings})
	} else if legacy == 0 {
		fmt.Println("herd legacy-receipts: no receiptless in-progress tasks")
	} else {
		fmt.Printf("herd legacy-receipts: %d receiptless in-progress task(s) (review/approve remains fail-closed)\n", legacy)
		for _, finding := range findings {
			if !finding.HasReceipt {
				state := "action required: re-dispatch through herd forge"
				if finding.Tombstoned {
					state = "tombstoned: " + finding.Reason
				}
				fmt.Printf("  %s %s — %s\n", finding.Ref, finding.Title, state)
			}
		}
	}
	if legacy > 0 {
		os.Exit(1)
	}
}
