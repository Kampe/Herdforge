package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// cannedTransport serves adapter-shaped responses in-process and records
// every request (count + last write body) — REAL adapter code paths run,
// no network.
type cannedTransport struct {
	mu       sync.Mutex
	reads    string // JSON body for read (GET) requests
	writes   string // JSON body for write (POST/PATCH/PUT) requests
	requests int
	// bodies records every request payload: GraphQL adapters send reads as
	// POSTs too, so readback assertions search all recorded bodies.
	bodies []string
	// comments is the board-like comment store used for exact effect
	// readback; shape selects the adapter's comment-list JSON form.
	comments []string
	shape    string
}

func (c *cannedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	body := c.reads
	isComment := strings.Contains(r.URL.Path, "/comment")
	if r.Method != http.MethodGet {
		raw := ""
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			raw = string(b)
		}
		c.bodies = append(c.bodies, raw)
		// GraphQL adapters route everything through one POST endpoint: the
		// operation decides whether this is a comment write or read.
		if c.shape == "linear" {
			switch {
			case strings.Contains(raw, "commentCreate"):
				if text := extractComment(c.shape, raw); text != "" {
					c.comments = append(c.comments, text)
				}
				body = `{"data":{"commentCreate":{"success":true}}}`
			case strings.Contains(raw, "IssueComments"):
				body = c.commentListBody()
			default:
				body = `{"data":{"issue":{"id":"task-id-1","identifier":"FAC-145","title":"t","priority":3,"state":{"name":"In Review"},"project":{"id":"proj-x"},"labels":{"nodes":[]}}}}`
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(body)),
				Request:    r,
			}, nil
		}
		if isComment || (c.shape == "azure" && strings.Contains(raw, "System.History")) {
			// Behave like a real board: the posted comment becomes visible
			// to subsequent comment reads (exact effect readback).
			if text := extractComment(c.shape, raw); text != "" {
				c.comments = append(c.comments, text)
			}
		}
		body = c.writes
	} else if isComment {
		body = c.commentListBody()
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    r,
	}, nil
}

func (c *cannedTransport) snapshot() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return c.requests, out
}

// extractComment recovers the comment text an adapter actually wrote, in
// that adapter's own request shape.
func extractComment(shape, raw string) string {
	switch shape {
	case "github":
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		return p.Body
	case "linear":
		var p struct {
			Variables struct {
				Body string `json:"body"`
			} `json:"variables"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		return p.Variables.Body
	case "jira":
		var p struct {
			Body struct {
				Content []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"content"`
			} `json:"body"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		var b strings.Builder
		for _, para := range p.Body.Content {
			for _, run := range para.Content {
				b.WriteString(run.Text)
			}
		}
		return b.String()
	case "azure":
		var ops []struct {
			Path  string `json:"path"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal([]byte(raw), &ops)
		for _, o := range ops {
			if strings.Contains(o.Path, "History") {
				return o.Value
			}
		}
		return ""
	default: // kaneo writes {"body": ...} and reads {"content": ...}
		var p struct {
			Body    string `json:"body"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		if p.Body != "" {
			return p.Body
		}
		return p.Content
	}
}

// commentListBody renders the stored comments in the adapter's own
// comment-list shape so REAL adapter parsing runs on readback.
// Caller holds c.mu.
func (c *cannedTransport) commentListBody() string {
	switch c.shape {
	case "github":
		list := make([]map[string]string, 0, len(c.comments))
		for _, cm := range c.comments {
			list = append(list, map[string]string{"body": cm})
		}
		out, _ := json.Marshal(list)
		return string(out)
	case "jira":
		type run struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		type para struct {
			Type    string `json:"type"`
			Content []run  `json:"content"`
		}
		type doc struct {
			Type    string `json:"type"`
			Content []para `json:"content"`
		}
		type comment struct {
			Body doc `json:"body"`
		}
		payload := struct {
			Comments []comment `json:"comments"`
		}{}
		for _, cm := range c.comments {
			payload.Comments = append(payload.Comments, comment{Body: doc{Type: "doc", Content: []para{{Type: "paragraph", Content: []run{{Type: "text", Text: cm}}}}}})
		}
		out, _ := json.Marshal(payload)
		return string(out)
	case "linear":
		nodes := make([]map[string]string, 0, len(c.comments))
		for _, cm := range c.comments {
			nodes = append(nodes, map[string]string{"body": cm})
		}
		out, _ := json.Marshal(map[string]any{"data": map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": nodes}}}})
		return string(out)
	case "azure":
		payload := struct {
			Comments []map[string]string `json:"comments"`
		}{}
		for _, cm := range c.comments {
			payload.Comments = append(payload.Comments, map[string]string{"text": cm})
		}
		out, _ := json.Marshal(payload)
		return string(out)
	default: // kaneo
		list := make([]map[string]string, 0, len(c.comments))
		for _, cm := range c.comments {
			list = append(list, map[string]string{"content": cm})
		}
		out, _ := json.Marshal(list)
		return string(out)
	}
}

// commentsSnapshot returns the board-visible comment bodies.
func (c *cannedTransport) commentsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.comments))
	copy(out, c.comments)
	return out
}

// sawBody reports whether ANY recorded request payload contains sub.
func (c *cannedTransport) sawBody(sub string) bool {
	_, bodies := c.snapshot()
	for _, b := range bodies {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return false
}

// NewSessionIDFor gives each conformance role its own session identity.
func NewSessionIDFor(role string) string { return role + "-conformance" }

// brokerRoundTrip drives ONE request through the real broker connection
// handler (serveBrokerConn) over an in-memory pipe.
func brokerRoundTrip(t *testing.T, root string, authority dispatch.BindingAuthority, verifier *dispatch.Verifier, signer *dispatch.Signer, tp provider.TaskProvider, req brokerRequest) brokerResponse {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveBrokerConn(server, root, &config.Config{}, authority, verifier, signer, tp)
	}()
	_ = client.SetDeadline(time.Now().Add(60 * time.Second))
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp brokerResponse
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	client.Close()
	<-done
	return resp
}

// FAC-145: REAL broker-backed conformance for EVERY production adapter —
// authorized bound reads through real adapter parsing, broker-composed
// verdict delivery with exact write readback, worker-note attribution, and
// unsupported-mutation refusal with zero additional adapter traffic. All
// receipts are backed by live claim-store leases.
func TestBrokerConformance_AllProviders(t *testing.T) {
	keyDir, root := t.TempDir(), t.TempDir()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, "herdforge", root)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	leaseStoreOpen := true
	closeLeaseStore := func() {
		if leaseStoreOpen {
			leaseStore.Close()
			leaseStoreOpen = false
		}
	}
	defer closeLeaseStore()

	// Adapter-shaped canned responses.
	githubTW := &cannedTransport{
		shape:  "github",
		reads:  `{"number":7,"title":"t","body":"b","state":"open","created_at":"2026-01-01T00:00:00Z","labels":[]}`,
		writes: `{}`,
	}
	linearTW := &cannedTransport{shape: "linear"}
	jiraTW := &cannedTransport{
		shape:  "jira",
		reads:  `{"id":"task-id-1","key":"FAC-145","fields":{"summary":"t","status":{"name":"In Review"},"priority":{"name":"Medium"},"labels":[],"created":"2026-01-01T00:00:00Z","project":{"key":"proj-x"}}}`,
		writes: `{}`,
	}
	azureTW := &cannedTransport{
		shape:  "azure",
		reads:  `{"id":9,"fields":{"System.Title":"t","System.State":"Active","System.WorkItemType":"Task","Microsoft.VSTS.Common.Priority":2,"System.CreatedDate":"2026-01-01T00:00:00Z"}}`,
		writes: `{}`,
	}
	kaneoTW := &cannedTransport{
		shape:  "kaneo",
		reads:  `{"id":"task-id-1","ref":"FAC-145","title":"t","status":"in-review","priority":"high","projectId":"proj-x","labels":[]}`,
		writes: `{}`,
	}

	github := provider.NewGitHubProvider("tok", "owner", "repo")
	github.Client = &http.Client{Transport: githubTW}
	linear := provider.NewLinearProvider("key")
	linear.Client = &http.Client{Transport: linearTW}
	jira := provider.NewJiraProvider("http://jira.internal", "u@example.com", "tok")
	jira.HTTPClient = &http.Client{Transport: jiraTW}
	azure := provider.NewAzureDevOpsProvider("http://azure.internal/org", "proj", "pat")
	azure.HTTPClient = &http.Client{Transport: azureTW}
	kaneo := provider.NewKaneoProvider("http://kaneo.internal", "proj-x", false)
	kaneo.Client = &http.Client{Transport: kaneoTW}
	memory := provider.NewMemoryProvider()
	memory.AddTask(&provider.Task{ID: "task-id-1", Ref: "FAC-145", Status: "in-review", ProjectID: "proj-x"})

	issueReceipt := func(providerType, role string, ops []string, candidate string) dispatch.TaskContext {
		key := claim.LeaseKey{Repo: "herdforge", Provider: providerType, Project: "proj-x", TaskRef: "FAC-145"}
		var leaseID string
		var leaseGen int64
		if leases, aErr := leaseStore.ActiveClaims(context.Background(), time.Now()); aErr == nil {
			for _, l := range leases {
				if l.LeaseKey == key {
					leaseID, leaseGen = fmt.Sprintf("claim:%d", l.ID), l.Generation
				}
			}
		}
		if leaseID == "" {
			lease, aErr := leaseStore.Acquire(context.Background(), key, "coordinator-test", role, "", time.Now(), time.Hour)
			if aErr != nil {
				t.Fatal(aErr)
			}
			leaseID, leaseGen = fmt.Sprintf("claim:%d", lease.ID), lease.Generation
		}
		tc := dispatch.TaskContext{
			ProviderType: providerType, ProjectID: "proj-x", Repository: "herdforge",
			Role: role, TaskRef: "FAC-145", TaskID: "task-id-1", Branch: "herd/fac-145",
			BaseSHA: "base123", CandidateSHA: candidate,
			LeaseID: leaseID, LeaseGeneration: leaseGen, LeaseTaskRef: "FAC-145", SessionID: NewSessionIDFor(role), AllowedOps: ops,
			ExpiresAt: time.Now().Add(time.Hour),
		}
		signed, sErr := signer.Issue(tc)
		if sErr != nil {
			t.Fatal(sErr)
		}
		return signed
	}
	authorityFor := func(providerType string) dispatch.BindingAuthority {
		return dispatch.BindingAuthority{Repository: "herdforge", ProviderType: providerType, ProjectID: "proj-x"}
	}

	// Issue every receipt up front, then release the store handle so broker
	// calls never contend with this test's own SQLite connection.
	type receiptPair struct{ worker, reviewer dispatch.TaskContext }
	receipts := map[string]receiptPair{}
	for _, name := range []string{"kaneo", "memory", "github", "linear", "jira", "azure"} {
		receipts[name] = receiptPair{
			worker:   issueReceipt(name, dispatch.RoleWorker, dispatch.WorkerOps, ""),
			reviewer: issueReceipt(name, dispatch.RoleReviewer, dispatch.ReviewerOps, "cafe1234beef"),
		}
	}
	closeLeaseStore()

	adapters := []struct {
		name     string
		tp       provider.TaskProvider
		tw       *cannedTransport
		readback bool // provider implements CommentReader (exact effect readback)
	}{
		{"kaneo", kaneo, kaneoTW, true}, {"memory", memory, nil, true},
		{"github", github, githubTW, true}, {"linear", linear, linearTW, true},
		{"jira", jira, jiraTW, true}, {"azure", azure, azureTW, true},
	}
	for _, a := range adapters {
		t.Run(a.name, func(t *testing.T) {
			worker, reviewer := receipts[a.name].worker, receipts[a.name].reviewer
			auth := authorityFor(a.name)

			// AUTHORIZED bound read through the real adapter parse path.
			resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: "get", Ref: "FAC-145", Receipt: worker})
			if !resp.OK {
				t.Fatalf("authorized own-task read must pass: %s", resp.Error)
			}
			if len(resp.Result) == 0 {
				t.Fatal("read must return the task")
			}

			// Worker note with broker-composed attribution reaches the
			// adapter's comment write path.
			resp = brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: "comment", Ref: "FAC-145", Body: "progress note", Receipt: worker})
			if !resp.OK {
				t.Fatalf("authorized worker note must pass: %s", resp.Error)
			}
			if a.tw != nil && !a.tw.sawBody("[note from worker FAC-145] progress note") {
				_, bodies := a.tw.snapshot()
				t.Fatalf("adapter must receive the attributed note, saw: %v", bodies)
			}

			// Typed verdict. Exactly-once REQUIRES provider effect readback:
			// adapters with CommentReader deliver and confirm; adapters
			// without it must FAIL CLOSED rather than publish an
			// unverifiable verdict (FAC-145).
			resp = brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{
				Op: "verdict", Ref: "FAC-145", Body: "REJECTED", CandidateSHA: "cafe1234beef", WorktreeHEAD: "cafe1234beef", Receipt: reviewer,
			})
			if a.readback {
				if !resp.OK {
					t.Fatalf("reviewer verdict must pass on a readback-capable adapter: %s", resp.Error)
				}
				if a.tw != nil && !a.tw.sawBody("REVIEW VERDICT FAC-145: REJECTED candidate=cafe1234beef") {
					_, bodies := a.tw.snapshot()
					t.Fatalf("adapter must receive the exact broker-composed verdict, saw: %v", bodies)
				}
				// EXACTLY ONCE: a retry re-reads the effect and delivers no
				// second comment.
				verdictCount := func() int {
					n := 0
					for _, c := range a.tw.commentsSnapshot() {
						if strings.Contains(c, "REVIEW VERDICT") {
							n++
						}
					}
					return n
				}
				beforeReq := 0
				if a.tw != nil {
					beforeReq = verdictCount()
				}
				if resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{
					Op: "verdict", Ref: "FAC-145", Body: "REJECTED", CandidateSHA: "cafe1234beef", WorktreeHEAD: "cafe1234beef", Receipt: reviewer,
				}); !resp.OK {
					t.Fatalf("verdict retry must be inert: %s", resp.Error)
				}
				if a.tw != nil {
					if after := verdictCount(); after != beforeReq {
						t.Fatalf("verdict retry duplicated the provider verdict: %d -> %d", beforeReq, after)
					}
				}
			} else if resp.OK {
				t.Fatal("adapter without comment readback must refuse to publish a verdict (FAC-145 fail-closed)")
			}

			// Unsupported mutation ops are refused BEFORE the adapter: the
			// request count must not move.
			var before int
			if a.tw != nil {
				before, _ = a.tw.snapshot()
			}
			for _, op := range []string{"update-status", "claim", "delete"} {
				if resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: op, Ref: "FAC-145", Receipt: worker}); resp.OK {
					t.Fatalf("unsupported op %q must be refused", op)
				}
			}
			// Tampered receipt, foreign ref, wrong candidate: refused, no
			// adapter traffic.
			forged := worker
			forged.Role = dispatch.RoleCoordinator
			if resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: "get", Ref: "FAC-145", Receipt: forged}); resp.OK {
				t.Fatal("tampered receipt must be refused")
			}
			if resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: "get", Ref: "FAC-999", Receipt: worker}); resp.OK {
				t.Fatal("foreign ref must be refused")
			}
			if resp := brokerRoundTrip(t, root, auth, verifier, signer, a.tp, brokerRequest{Op: "verdict", Ref: "FAC-145", Body: "APPROVED", CandidateSHA: "other", WorktreeHEAD: "cafe1234beef", Receipt: reviewer}); resp.OK {
				t.Fatal("wrong-candidate verdict must be refused")
			}
			if a.tw != nil {
				after, _ := a.tw.snapshot()
				if after != before {
					t.Fatalf("refused operations reached the adapter: %d -> %d requests", before, after)
				}
			}
		})
	}

	if task, _ := memory.GetTask(context.Background(), "task-id-1"); task == nil || task.Status != "in-review" {
		t.Fatalf("memory adapter state must be untouched by refused ops: %+v", task)
	}

	// Supersession on the durable bus: REJECTED (posted after any earlier
	// APPROVED for the same candidate) is the effective verdict.
	mb := mail.NewMailbox(filepath.Join(root, mail.DefaultMailFile))
	eff, found, err := mb.EffectiveVerdict("herdforge", "FAC-145", "cafe1234beef")
	if err != nil || !found {
		t.Fatalf("verdict record must exist for readback-capable adapters: found=%v err=%v", found, err)
	}
	if eff.Kind != mail.CallbackBlocked {
		t.Fatalf("REJECTED must be the effective verdict, got %s", eff.Kind)
	}
}
