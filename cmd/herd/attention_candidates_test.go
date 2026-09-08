package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

func TestAttentionPRSnapshotRefusesIncompleteEvidence(t *testing.T) {
	for _, body := range []string{"null", `{ "error": "provider failed" }`, `[{"number":42}]`, "not-json"} {
		_, err := readAttentionPRs(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return []byte(body), nil }, "herd/fac-598")
		if err == nil {
			t.Fatalf("accepted incomplete snapshot %s", body)
		}
	}
	_, err := readAttentionPRs(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`[]`), errors.New("provider timeout")
	}, "herd/fac-598")
	if err == nil {
		t.Fatal("producer failure became no PR")
	}
	var views []prView
	for i := 0; i < 100; i++ {
		views = append(views, prView{Number: i + 1, HeadRefOid: strings.Repeat("a", 40), State: "OPEN", URL: "https://example.test/pr"})
	}
	body, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAttentionPRs(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return body, nil }, "herd/fac-598"); err == nil {
		t.Fatal("limit-sized result was considered complete")
	}
}

type attentionTaskReader struct {
	provider.TaskProvider // any unexpected mutation/read is an immediate test failure
	task                  *provider.Task
	err                   error
	calls                 int
}

func (p *attentionTaskReader) GetTask(ctx context.Context, ref string) (*provider.Task, error) {
	p.calls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("task read has no deadline")
	}
	if ref != "FAC-598" {
		return nil, errors.New("wrong task ref")
	}
	return p.task, p.err
}

func TestCollectAttentionCandidatesUsesCanonicalEvidence(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, variant := range []string{"ready", "no-pr", "timeout", "wrong-project", "moved-head", "done", "consumed", "failed-review", "callback-blocked", "callback-complete", "callback-wrong-lease", "verdict-intent", "landing-receipt", "superseded", "callback-unbound"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
			t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
			t.Setenv("HERD_MAIL_FILE", filepath.Join(root, ".herd", "mail.jsonl"))
			ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			record := `{"event":"record","sha":"` + sha + `","task":"FAC-598","branch":"herd/fac-598","reviewer":"reviewer","builder_family":"openai","reviewer_family":"anthropic","gate":"independent"}`
			verdict := "PASS"
			if variant == "failed-review" {
				verdict = "FAIL"
			}
			body := record + "\n" + `{"event":"verdict","sha":"` + sha + `","reviewer":"reviewer","builder_family":"openai","reviewer_family":"anthropic","verdict":"` + verdict + `"}` + "\n"
			if variant == "superseded" {
				body += `{"event":"supersession","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","task":"` + sha + `","status":"superseded"}` + "\n"
			}
			if err := os.WriteFile(ledgerPath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			queue := `{"event":"enqueue","sha":"` + sha + `","branch":"herd/fac-598"}` + "\n"
			if variant == "consumed" {
				queue += `{"event":"consumed","sha":"` + sha + `"}` + "\n"
			}
			if err := os.WriteFile(ledger.QueuePath, []byte(queue), 0600); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(variant, "callback-") || variant == "verdict-intent" {
				cb := mail.Callback{Ref: "FAC-598", SHA: sha, Kind: mail.CallbackBlocked, LeaseGeneration: 4}
				if variant == "callback-unbound" {
					cb.SHA = ""
				}
				if variant == "verdict-intent" {
					cb.DedupeID = mail.VerdictIntentID("write-ahead")
				}
				encode := func(seq int64) []byte {
					body, err := json.Marshal(cb)
					if err != nil {
						t.Fatal(err)
					}
					line, err := json.Marshal(mail.Envelope{Sequence: seq, Recipient: mail.CoordinatorInbox, Body: string(body)})
					if err != nil {
						t.Fatal(err)
					}
					return append(line, '\n')
				}
				lines := encode(1)
				if variant == "callback-complete" || variant == "callback-wrong-lease" {
					cb.Kind = mail.CallbackComplete
					if variant == "callback-wrong-lease" {
						cb.LeaseGeneration = 5
					}
					lines = append(lines, encode(2)...)
				}
				if err := os.WriteFile(mail.CallbackMailPath(root), lines, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "landing-receipt" {
				receipt := hsync.CompletionReceipt{TaskRef: "FAC-598", CandidateSHA: sha}
				receipt.Digest = receipt.ComputeDigest()
				path := hsync.ReceiptPath(root, "FAC-598")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{}
			cfg.TaskProvider.ProjectID = "project"
			tp := &attentionTaskReader{task: &provider.Task{Ref: "FAC-598", ProjectID: "project", Status: "in-review"}}
			if variant == "timeout" {
				tp.err = context.DeadlineExceeded
			}
			if variant == "wrong-project" {
				tp.task.ProjectID = "other"
			}
			if variant == "done" {
				tp.task.Status = "done"
			}
			ghCalls := 0
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "git" {
					if variant == "moved-head" {
						return []byte(strings.Repeat("b", 40)), nil
					}
					return []byte(sha), nil
				}
				if name != "gh" {
					t.Fatalf("unexpected command %s", name)
				}
				ghCalls++
				if variant == "no-pr" {
					return []byte(`[]`), nil
				}
				return []byte(`[{"number":42,"headRefOid":"` + sha + `","state":"OPEN","url":"https://example.test/pr/42","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"Build, Preflight & Test Suite","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://example.test/check/1"}]}]`), nil
			}
			got, err := collectAttentionCandidates(context.Background(), root, cfg, tp, run)
			switch variant {
			case "timeout", "wrong-project", "callback-unbound":
				if err == nil || len(got) != 1 || got[0].Status != "ready-evidence-unknown" || ghCalls != 0 {
					t.Fatalf("failed identity read: findings=%+v err=%v gh=%d", got, err, ghCalls)
				}
			case "consumed", "failed-review", "moved-head", "done", "superseded":
				if err != nil || len(got) != 0 || ghCalls != 0 {
					t.Fatalf("stale finding=%+v err=%v gh=%d", got, err, ghCalls)
				}
			case "landing-receipt":
				if err != nil || len(got) != 1 || got[0].Status != "ready-landing-recorded" || ghCalls != 0 {
					t.Fatalf("landing observation=%+v err=%v gh=%d", got, err, ghCalls)
				}
			default:
				want := "ready-but-open"
				if variant == "callback-blocked" || variant == "callback-wrong-lease" {
					want = "ready-callback-blocked"
				}
				if variant == "no-pr" {
					want = "ready-without-pr"
				}
				if err != nil || len(got) != 1 || got[0].Status != want || got[0].SHA != sha || ghCalls != 1 {
					t.Fatalf("findings=%+v err=%v gh=%d", got, err, ghCalls)
				}
			}
		})
	}
}

func TestSameHostRetryReachesAttentionPipeline(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	root := t.TempDir()
	ledgerPath := filepath.Join(root, ".herd", "review-ledger.jsonl")
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	t.Setenv("HERD_MAIL_FILE", filepath.Join(root, ".herd", "mail.jsonl"))
	ledger, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"event":"record","sha":"` + sha + `","task":"FAC-598","branch":"herd/fac-598","reviewer":"reviewer-a","host":"host-a","builder_family":"openai","reviewer_family":"anthropic","gate":"independent"}` + "\n" +
		`{"event":"record","sha":"` + sha + `","task":"FAC-598","branch":"herd/fac-598","reviewer":"reviewer-b","host":"host-a","builder_family":"openai","reviewer_family":"anthropic","gate":"independent"}` + "\n" +
		`{"event":"verdict","sha":"` + sha + `","reviewer":"reviewer-a","host":"host-a","builder_family":"openai","reviewer_family":"anthropic","verdict":"FAIL"}` + "\n" +
		`{"event":"verdict","sha":"` + sha + `","reviewer":"reviewer-b","host":"host-a","builder_family":"openai","reviewer_family":"anthropic","verdict":"PASS","retry_of":"reviewer-a"}` + "\n" +
		`{"event":"supersession","sha":"` + sha + `","task":"` + sha + `","reviewer":"reviewer-a","host":"host-a","retry_of":"reviewer-a","status":"superseded"}` + "\n"
	if err := os.WriteFile(ledgerPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger.QueuePath, []byte(`{"event":"enqueue","sha":"`+sha+`","branch":"herd/fac-598"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.TaskProvider.ProjectID = "project"
	tp := &attentionTaskReader{task: &provider.Task{Ref: "FAC-598", ProjectID: "project", Status: "in-review"}}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte(sha), nil
		}
		if name != "gh" {
			t.Fatalf("unexpected command %s", name)
		}
		return []byte(`[{"number":42,"headRefOid":"` + sha + `","state":"OPEN","url":"https://example.test/pr/42","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"Build, Preflight & Test Suite","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://example.test/check/1"}]}]`), nil
	}
	got, err := collectAttentionCandidates(context.Background(), root, cfg, tp, run)
	if err != nil || len(got) != 1 || got[0].SHA != sha {
		t.Fatalf("same-host retry attention empty: findings=%+v err=%v", got, err)
	}
}
