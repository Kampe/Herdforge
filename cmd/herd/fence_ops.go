package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-785 exact fenced-operation readback.
//
// herd fence-op status <opID> is READ-ONLY: it reads the local claim-dir
// fence store and (when HERD_FENCE_BROKER_URL is configured) performs the
// broker's worker-safe GET /v1/ops/<opID> readback, then emits the exact
// op/task/project/repository identity with an honest applied/ambiguous/
// unknown state. It never mutates a provider, never mints, and never
// creates bookkeeping records.
//
// herd fence-op reconcile is REPORT-ONLY by default: it enumerates pending
// provider-transition outbox records, verifies each against op-bound
// upstream proof only, and reports which would settle. It never settles.
//
// --settle is explicitly UNAVAILABLE in this slice. No existing
// non-forgeable coordinator credential can be delivered to a one-shot CLI
// process: the claim-dir mint credential is blocked pending FAC-169 (a
// same-UID 0600 file is not an authority boundary), environment/role
// strings are forgeable and are never a positive authority grant, and the
// broker worker token is worker-safe by design. The missing prerequisite is
// a coordinator-only credential channel (FAC-169/FAC-652 ownership) wired
// to this command; until it exists, settlement stays refused before any
// evaluation and local bookkeeping is untouched.

const fenceOpSettleRefusal = `herd fence-op: --settle is unavailable: no existing non-forgeable coordinator ` +
	`authority can be delivered to a one-shot CLI process (claim-dir mint credential is blocked pending ` +
	`FAC-169; env/role strings are forgeable and are not authority; the broker worker token is worker-safe). ` +
	`Missing prerequisite: a coordinator-only credential channel (FAC-169/FAC-652) wired to this command. ` +
	`No local bookkeeping was changed; records remain exactly as they were.`

// fenceOpExit codes (documented in the usage string):
// 0 proven applied; 1 error/refusal; 3 ambiguous; 4 unknown.
const (
	fenceOpExitApplied   = 0
	fenceOpExitError     = 1
	fenceOpExitAmbiguous = 3
	fenceOpExitUnknown   = 4
)

func runFenceOps() {
	args := os.Args[2:]
	if len(args) == 0 {
		fenceOpUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "status":
		runFenceOpStatus()
	case "reconcile":
		runFenceOpReconcile()
	case "--help", "-h", "help":
		fenceOpUsage()
	default:
		fmt.Fprintf(os.Stderr, "herd fence-op: unknown subcommand %q\n", args[0])
		fenceOpUsage()
		os.Exit(2)
	}
}

func fenceOpUsage() {
	fmt.Println("Usage: herd fence-op status <opID> [--repo R] [--project P] [--task-ref TR] [--task T] [--json]")
	fmt.Println("  Read-only exact-operation readback: local fence store + broker GET /v1/ops/<opID>.")
	fmt.Println("  Never mutates a provider or local bookkeeping. Exit: 0 applied, 1 error/refusal,")
	fmt.Println("  3 ambiguous, 4 unknown.")
	fmt.Println("Usage: herd fence-op reconcile [--op <opID>] [--settle] [--json]")
	fmt.Println("  Report-only by default: verifies pending provider-transition records against")
	fmt.Println("  op-bound upstream proof only and reports what would settle. Exit: 0 nothing")
	fmt.Println("  pending, 1 error/refusal, 3 work remains, 4 unknown op. --settle is refused:")
	fmt.Println("  no coordinator authority primitive exists for a one-shot CLI.")
}

func resolveFenceOpClaimDir() (string, error) {
	override := os.Getenv("HERD_ROOT")
	if override == "" {
		override = os.Getenv("HERD_REPO_ROOT")
	}
	return provider.CanonicalClaimDir(".", override)
}

// fenceOpStorePaths returns the durable store paths that exist. A missing
// claim dir is a hard error; missing individual store files mean "no local
// evidence of that kind" and are reported honestly rather than fabricated.
func fenceOpStorePaths(claimDir string) (fencePath, outboxPath string, err error) {
	fi, ferr := os.Stat(claimDir)
	if ferr != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("fence-op: claim dir %s not found (set HERD_CLAIM_DIR); refusing to guess", claimDir)
	}
	fencePath = filepath.Join(claimDir, "fences.db")
	outboxPath = filepath.Join(claimDir, "outbox.db")
	if _, err := os.Stat(fencePath); err != nil {
		fencePath = ""
	}
	if _, err := os.Stat(outboxPath); err != nil {
		outboxPath = ""
	}
	return fencePath, outboxPath, nil
}

func openFenceOpStores(fencePath, outboxPath string) (*provider.SQLiteFenceStore, *claim.SQLiteOutbox, error) {
	var fences *provider.SQLiteFenceStore
	var outbox *claim.SQLiteOutbox
	var firstErr error
	if fencePath != "" {
		fs, err := provider.NewSQLiteFenceStore(fencePath)
		if err != nil {
			firstErr = err
		} else {
			fences = fs
		}
	}
	if outboxPath != "" {
		ob, err := claim.NewSQLiteOutbox(outboxPath)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if fences != nil {
				_ = fences.Close()
				fences = nil
			}
		} else {
			outbox = ob
		}
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return fences, outbox, nil
}

// parsedIntentKey is the identity bound into a provider-transition outbox
// record's idempotency key: provider:<repo>/<provider>/<project>/<taskRef>:g<gen>:<kind>.
// Repo may itself contain slashes, so parsing goes right-to-left and refuses
// anything that does not match the exact shape (never guessed).
type parsedIntentKey struct {
	Raw        string `json:"raw"`
	Repo       string `json:"repo"`
	Provider   string `json:"provider"`
	Project    string `json:"project"`
	TaskRef    string `json:"task_ref"`
	Generation int64  `json:"generation"`
	Kind       string `json:"kind"`
}

func parseProviderIntentKey(key string) (*parsedIntentKey, bool) {
	rest, found := strings.CutPrefix(key, "provider:")
	if !found {
		return nil, false
	}
	// The tail is ":g<generation>:<kind>", and kind may itself contain ':'
	// (e.g. status:done), so locate the LAST ":g<digits>:" delimiter by
	// scanning right to left and take everything before it as identity.
	for idx := strings.LastIndex(rest, ":g"); idx >= 0; idx = strings.LastIndex(rest[:idx], ":g") {
		tail := rest[idx+2:]
		digitEnd := 0
		for digitEnd < len(tail) && tail[digitEnd] >= '0' && tail[digitEnd] <= '9' {
			digitEnd++
		}
		if digitEnd == 0 || digitEnd >= len(tail) || tail[digitEnd] != ':' {
			continue
		}
		gen, gerr := strconv.ParseInt(tail[:digitEnd], 10, 64)
		if gerr != nil || gen < 0 {
			continue
		}
		kind := tail[digitEnd+1:]
		head := rest[:idx]
		parts := strings.Split(head, "/")
		if len(parts) < 4 || kind == "" {
			continue
		}
		taskRef := parts[len(parts)-1]
		project := parts[len(parts)-2]
		prov := parts[len(parts)-3]
		repo := strings.Join(parts[:len(parts)-3], "/")
		if repo == "" || prov == "" || project == "" || taskRef == "" {
			continue
		}
		return &parsedIntentKey{
			Raw: key, Repo: repo, Provider: prov, Project: project,
			TaskRef: taskRef, Generation: gen, Kind: kind,
		}, true
	}
	return nil, false
}

// fenceOpEvidence is the combined read-only view of one operation. The
// top-level receipt fields mirror the broker's GET /v1/ops/<opID> shape so
// consumers get one stable identity surface.
type fenceOpEvidence struct {
	OpID             string                    `json:"op_id"`
	State            string                    `json:"state"` // applied | ambiguous | unknown
	Applied          bool                      `json:"applied"`
	Ambiguous        bool                      `json:"ambiguous"`
	TaskID           string                    `json:"task_id,omitempty"`
	FenceToken       int64                     `json:"fence_token,omitempty"`
	ExpectedStatus   string                    `json:"expected_status,omitempty"`
	Revision         string                    `json:"revision,omitempty"`
	Receipt          *provider.FenceOpReadback `json:"receipt,omitempty"`
	Local            *provider.FenceOpReadback `json:"local_receipt,omitempty"`
	Broker           *provider.FenceOpReadback `json:"broker_receipt,omitempty"`
	BrokerConfigured bool                      `json:"broker_configured"`
	Outbox           *fenceOpOutboxView        `json:"outbox,omitempty"`
	Refusals         []string                  `json:"refusals,omitempty"`
}

type fenceOpOutboxView struct {
	IdempotencyKey string           `json:"idempotency_key"`
	Kind           string           `json:"kind"`
	Status         string           `json:"status"`
	Attempts       int              `json:"attempts"`
	LastError      string           `json:"last_error,omitempty"`
	Identity       *parsedIntentKey `json:"identity,omitempty"`
}

func localReceiptAsReadback(rc *provider.OpReceipt) *provider.FenceOpReadback {
	if rc == nil {
		return nil
	}
	return &provider.FenceOpReadback{
		Applied:        !rc.Ambiguous,
		Ambiguous:      rc.Ambiguous,
		OpID:           rc.OpID,
		TaskID:         rc.TaskID,
		FenceToken:     rc.FenceToken,
		ExpectedStatus: rc.ExpectedStatus,
		Revision:       rc.Revision,
	}
}

// findOutboxRecord scans provider-transition records for the exact
// operation payload across all lifecycle states. Read-only.
func findOutboxRecord(ctx context.Context, outbox *claim.SQLiteOutbox, opID string) (*fenceOpOutboxView, error) {
	if outbox == nil || opID == "" {
		return nil, nil
	}
	rec, err := outbox.FindByPayload(ctx, []byte(opID))
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	if !claim.IsProviderTransitionKind(rec.Kind) {
		return nil, nil
	}
	view := &fenceOpOutboxView{
		IdempotencyKey: rec.IdempotencyKey,
		Kind:           rec.Kind,
		Status:         string(rec.Status),
		Attempts:       rec.Attempts,
		LastError:      rec.LastError,
	}
	if pk, ok := parseProviderIntentKey(rec.IdempotencyKey); ok {
		view.Identity = pk
	}
	return view, nil
}

// checkReceiptConsistency verifies that all available receipts for an operation
// agree on task identity, op identity, status, and fence token. Any contradiction
// is a hard refusal (fail-closed).
func checkReceiptConsistency(local, broker *provider.FenceOpReadback) []string {
	if local == nil || broker == nil {
		return nil
	}
	var refusals []string
	if local.TaskID != "" && broker.TaskID != "" && local.TaskID != broker.TaskID {
		refusals = append(refusals, fmt.Sprintf("task binding mismatch: local receipt task %q != broker receipt task %q", local.TaskID, broker.TaskID))
	}
	if local.OpID != "" && broker.OpID != "" && !strings.EqualFold(local.OpID, broker.OpID) {
		refusals = append(refusals, fmt.Sprintf("op binding mismatch: local receipt op %q != broker receipt op %q", local.OpID, broker.OpID))
	}
	if local.ExpectedStatus != "" && broker.ExpectedStatus != "" && provider.NormalizeStatus(local.ExpectedStatus) != provider.NormalizeStatus(broker.ExpectedStatus) {
		refusals = append(refusals, fmt.Sprintf("expected-status binding mismatch: local %q != broker %q", local.ExpectedStatus, broker.ExpectedStatus))
	}
	if local.FenceToken > 0 && broker.FenceToken > 0 && local.FenceToken != broker.FenceToken {
		refusals = append(refusals, fmt.Sprintf("fence-token binding mismatch: local %d != broker %d", local.FenceToken, broker.FenceToken))
	}
	return refusals
}

// fenceOpBindingRefusals checks caller-asserted bindings against evidence and
// verifies that all local/broker receipts and outbox identity bindings are consistent.
// Any mismatch or unconfirmed assertion is a refusal (fail closed), never a silent pass.
func fenceOpBindingRefusals(ev *fenceOpEvidence, wantRepo, wantProject, wantTaskRef, wantTask string) []string {
	var refusals []string
	refusals = append(refusals, checkReceiptConsistency(ev.Local, ev.Broker)...)

	id := (*parsedIntentKey)(nil)
	if ev.Outbox != nil {
		id = ev.Outbox.Identity
	}
	if wantRepo != "" && (id == nil || id.Repo != wantRepo) {
		refusals = append(refusals, "repo binding mismatch")
	}
	if wantProject != "" && (id == nil || id.Project != wantProject) {
		refusals = append(refusals, "project binding mismatch")
	}
	if wantTaskRef != "" && (id == nil || id.TaskRef != wantTaskRef) {
		refusals = append(refusals, "task-ref binding mismatch")
	}
	if wantTask != "" {
		hasReceipt := false
		for _, rc := range []*provider.FenceOpReadback{ev.Local, ev.Broker} {
			if rc != nil {
				hasReceipt = true
				if rc.TaskID != wantTask {
					refusals = append(refusals, fmt.Sprintf("task binding mismatch: receipt task %q != %q", rc.TaskID, wantTask))
				}
			}
		}
		if !hasReceipt {
			refusals = append(refusals, "task binding mismatch: no receipt available to confirm task")
		}
	}
	return refusals
}

// combineFenceOpEvidence resolves the honest overall state. Applied wins
// over ambiguous (receipts are monotonic) only when all available evidence is
// consistent. Any inconsistency forces the state to unknown.
func combineFenceOpEvidence(local, broker *provider.FenceOpReadback) (state string) {
	if len(checkReceiptConsistency(local, broker)) > 0 {
		return "unknown"
	}
	for _, rc := range []*provider.FenceOpReadback{local, broker} {
		if rc == nil {
			continue
		}
		if rc.Applied && !rc.Ambiguous {
			return "applied"
		}
	}
	for _, rc := range []*provider.FenceOpReadback{local, broker} {
		if rc != nil && rc.Ambiguous {
			return "ambiguous"
		}
	}
	return "unknown"
}

// fenceOpFlags is a tiny args scanner for fence-op subcommands. Go's flag
// package stops parsing at the first non-flag token, so the natural usage
// `fence-op status <opID> --json` would strand --json as a positional. This
// scanner accepts flags and the positional in any order and refuses
// unknown flags (fail closed), so usage drift cannot widen the surface.
type fenceOpFlags struct {
	values     map[string]string
	positional []string
}

func parseFenceOpArgs(args []string, boolFlags, valueFlags map[string]bool) (*fenceOpFlags, error) {
	out := &fenceOpFlags{values: map[string]string{}}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			out.positional = append(out.positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(tok, "-") {
			name := strings.TrimLeft(tok, "-")
			var ok bool
			if boolFlags[name] {
				ok = true
				out.values[name] = "true"
			} else if valueFlags[name] {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag %s requires a value", tok)
				}
				i++
				ok = true
				out.values[name] = args[i]
			}
			if !ok {
				return nil, fmt.Errorf("unknown flag %s", tok)
			}
			continue
		}
		out.positional = append(out.positional, tok)
	}
	return out, nil
}

func (f *fenceOpFlags) str(name string) string   { return f.values[name] }
func (f *fenceOpFlags) boolean(name string) bool { return f.values[name] == "true" }

func runFenceOpStatus() {
	boolFlags := map[string]bool{"json": true}
	valueFlags := map[string]bool{"repo": true, "project": true, "task-ref": true, "task": true}
	parsed, perr := parseFenceOpArgs(os.Args[3:], boolFlags, valueFlags)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: %v\n", perr)
		fenceOpUsage()
		os.Exit(2)
	}
	asJSON := parsed.boolean("json")
	wantRepo := parsed.str("repo")
	wantProject := parsed.str("project")
	wantTaskRef := parsed.str("task-ref")
	wantTask := parsed.str("task")

	if len(parsed.positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd fence-op status <opID> [--json] [--repo R] [--project P] [--task-ref TR] [--task T]")
		os.Exit(2)
	}
	opID := parsed.positional[0]
	if err := provider.ValidateOpID(opID); err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: %v\n", err)
		os.Exit(fenceOpExitError)
	}

	ctx := context.Background()
	claimDir, err := resolveFenceOpClaimDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	fencePath, outboxPath, err := fenceOpStorePaths(claimDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	fences, outbox, err := openFenceOpStores(fencePath, outboxPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	if fences != nil {
		defer fences.Close()
	}
	if outbox != nil {
		defer outbox.Close()
	}

	var localRC *provider.FenceOpReadback
	if fences != nil {
		rc, err := fences.LookupApplied(ctx, opID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd fence-op status: local fence store: %v\n", err)
			os.Exit(fenceOpExitError)
		}
		localRC = localReceiptAsReadback(rc)
	}

	var brokerRC *provider.FenceOpReadback
	brokerConfigured := false
	if url := strings.TrimSpace(os.Getenv("HERD_FENCE_BROKER_URL")); url != "" {
		brokerConfigured = true
		client, cerr := provider.NewFenceBrokerClientFromEnv()
		if cerr != nil {
			// Fail closed: a configured-but-unusable broker is an error,
			// never a silent local-only fallback.
			fmt.Fprintf(os.Stderr, "herd fence-op status: broker configured but unusable: %v\n", cerr)
			os.Exit(fenceOpExitError)
		}
		rc, rerr := client.LookupOp(ctx, opID)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "herd fence-op status: broker readback unavailable (fail closed): %v\n", rerr)
			os.Exit(fenceOpExitError)
		}
		brokerRC = rc
	}

	outboxView, err := findOutboxRecord(ctx, outbox, opID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op status: outbox: %v\n", err)
		os.Exit(fenceOpExitError)
	}

	ev := &fenceOpEvidence{
		OpID:             opID,
		BrokerConfigured: brokerConfigured,
	}
	if localRC != nil {
		ev.Local = localRC
		ev.Receipt = localRC
	}
	if brokerRC != nil {
		ev.Broker = brokerRC
		if ev.Receipt == nil || (ev.Receipt.Ambiguous && !brokerRC.Ambiguous) {
			ev.Receipt = brokerRC
		}
	}
	ev.State = combineFenceOpEvidence(localRC, brokerRC)
	switch ev.State {
	case "applied":
		ev.Applied, ev.Ambiguous = true, false
	case "ambiguous":
		ev.Applied, ev.Ambiguous = false, true
	default:
		ev.Applied, ev.Ambiguous = false, false
	}
	if ev.Receipt != nil {
		ev.TaskID = ev.Receipt.TaskID
		ev.FenceToken = ev.Receipt.FenceToken
		ev.ExpectedStatus = ev.Receipt.ExpectedStatus
		ev.Revision = ev.Receipt.Revision
	}
	if outboxView != nil {
		ev.Outbox = outboxView
	}

	refusals := fenceOpBindingRefusals(ev, wantRepo, wantProject, wantTaskRef, wantTask)
	if len(refusals) > 0 {
		ev.Refusals = refusals
		ev.State = "unknown"
		ev.Applied = false
		ev.Ambiguous = false
		ev.Receipt = nil
		ev.TaskID = ""
		emitFenceOp(ev, asJSON)
		for _, r := range refusals {
			fmt.Fprintf(os.Stderr, "herd fence-op status: %s: %s\n", opID, r)
		}
		os.Exit(fenceOpExitError)
	}

	emitFenceOp(ev, asJSON)
	switch ev.State {
	case "applied":
		os.Exit(fenceOpExitApplied)
	case "ambiguous":
		os.Exit(fenceOpExitAmbiguous)
	default:
		os.Exit(fenceOpExitUnknown)
	}
}

// fenceOpVerdict is the report-only reconcile verdict for one record.
type fenceOpVerdict struct {
	OpID           string             `json:"op_id"`
	Outbox         *fenceOpOutboxView `json:"outbox,omitempty"`
	Verdict        string             `json:"verdict"` // proven-applied | not-proven
	Reason         string             `json:"reason,omitempty"`
	TaskID         string             `json:"task_id,omitempty"`
	ExpectedStatus string             `json:"expected_status,omitempty"`
	Ambiguous      bool               `json:"ambiguous"`
}

type fenceOpReconcileReport struct {
	Records      []fenceOpVerdict `json:"records"`
	WouldSettle  int              `json:"would_settle"`
	StillPending int              `json:"still_pending"`
	Settled      int              `json:"settled"`
}

func runFenceOpReconcile() {
	boolFlags := map[string]bool{"json": true, "settle": true}
	valueFlags := map[string]bool{"op": true}
	parsed, perr := parseFenceOpArgs(os.Args[3:], boolFlags, valueFlags)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: %v\n", perr)
		fenceOpUsage()
		os.Exit(2)
	}
	asJSON := parsed.boolean("json")
	wantOp := parsed.str("op")
	settle := parsed.boolean("settle")

	if settle {
		// Refuse before ANY store access: zero mutations, zero reads of
		// bookkeeping, no evaluation that could be mistaken for authorization.
		fmt.Fprintln(os.Stderr, fenceOpSettleRefusal)
		os.Exit(fenceOpExitError)
	}

	ctx := context.Background()
	claimDir, err := resolveFenceOpClaimDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	fencePath, outboxPath, err := fenceOpStorePaths(claimDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	if fencePath == "" || outboxPath == "" {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: claim dir %s is missing fences.db/outbox.db; nothing to reconcile (never fabricating stores)\n", claimDir)
		os.Exit(fenceOpExitError)
	}
	fences, outbox, err := openFenceOpStores(fencePath, outboxPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	defer fences.Close()
	defer outbox.Close()

	pending, err := outbox.Pending(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: %v\n", err)
		os.Exit(fenceOpExitError)
	}

	// Broker client is built lazily and ONLY for the readback GET.
	var brokerClient *provider.FenceBrokerClient
	if url := strings.TrimSpace(os.Getenv("HERD_FENCE_BROKER_URL")); url != "" {
		client, cerr := provider.NewFenceBrokerClientFromEnv()
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "herd fence-op reconcile: broker configured but unusable: %v\n", cerr)
			os.Exit(fenceOpExitError)
		}
		brokerClient = client
	}

	report := fenceOpReconcileReport{Settled: 0}
	sawLocalStoreError := false
	matched := 0
	for _, rec := range pending {
		if !claim.IsProviderTransitionKind(rec.Kind) {
			continue
		}
		opID := strings.TrimSpace(string(rec.Payload))
		if wantOp != "" && opID != wantOp {
			continue
		}
		matched++
		v := fenceOpVerdict{
			OpID:    opID,
			Verdict: "not-proven",
			Reason:  "no-op-bound-receipt",
		}
		if recView := fenceOpOutboxViewFor(rec); recView != nil {
			v.Outbox = recView
		}

		localApplied, localAmbiguous := false, false
		var localRC *provider.OpReceipt
		if opID != "" {
			rc, lerr := fences.LookupApplied(ctx, opID)
			if lerr != nil {
				v.Reason = "local-fence-store-error"
				v.Verdict = "not-proven"
				sawLocalStoreError = true // degrade exit, do not silently pass
			} else if rc != nil {
				localRC = rc
				localApplied, localAmbiguous = !rc.Ambiguous, rc.Ambiguous
				v.TaskID, v.ExpectedStatus = rc.TaskID, rc.ExpectedStatus
				v.Ambiguous = rc.Ambiguous
			}
		} else {
			v.Reason = "record-has-no-op-payload"
		}

		if opID != "" && brokerClient != nil {
			brc, berr := brokerClient.LookupOp(ctx, opID)
			if berr != nil {
				// Unavailable upstream fails closed (acceptance: nonzero exit).
				fmt.Fprintf(os.Stderr, "herd fence-op reconcile: broker readback unavailable (fail closed): %v\n", berr)
				os.Exit(fenceOpExitError)
			}
			if brc != nil {
				if localRC != nil {
					localReadback := localReceiptAsReadback(localRC)
					if inconsistencies := checkReceiptConsistency(localReadback, brc); len(inconsistencies) > 0 {
						v.Verdict = "not-proven"
						v.Reason = "task-binding-mismatch"
						v.Ambiguous = false
						localApplied = false
						localAmbiguous = false
						goto recordEvaluated
					}
				}
				v.TaskID, v.ExpectedStatus, v.Ambiguous = brc.TaskID, brc.ExpectedStatus, brc.Ambiguous
				if brc.Applied && !brc.Ambiguous {
					localApplied = true
					localAmbiguous = false
				} else if brc.Ambiguous {
					localAmbiguous = true
				} else if !localApplied && !localAmbiguous {
					v.Reason = "broker-says-not-applied"
				}
			}
		}

	recordEvaluated:
		switch {
		case localApplied:
			v.Verdict = "proven-applied"
			v.Reason = ""
		case localAmbiguous:
			v.Verdict = "not-proven"
			if v.Reason == "no-op-bound-receipt" || v.Reason == "broker-says-not-applied" {
				v.Reason = "receipt-ambiguous"
			}
		}
		report.Records = append(report.Records, v)
		if v.Verdict == "proven-applied" {
			report.WouldSettle++
		} else {
			report.StillPending++
		}
	}

	if wantOp != "" && matched == 0 {
		// Exact-op discipline: an unknown op is unknown, never "settled".
		fmt.Fprintf(os.Stderr, "herd fence-op reconcile: op %s has no pending provider-transition record (unknown)\n", wantOp)
		os.Exit(fenceOpExitUnknown)
	}
	if sawLocalStoreError {
		os.Exit(fenceOpExitError)
	}

	if report.Records == nil {
		report.Records = []fenceOpVerdict{}
	}
	emitFenceOp(report, asJSON)
	if report.WouldSettle+report.StillPending > 0 {
		os.Exit(fenceOpExitAmbiguous) // findings-style exit: work remains
	}
	os.Exit(fenceOpExitApplied)
}

func fenceOpOutboxViewFor(rec *claim.OutboxRecord) *fenceOpOutboxView {
	view := &fenceOpOutboxView{
		IdempotencyKey: rec.IdempotencyKey,
		Kind:           rec.Kind,
		Status:         string(rec.Status),
		Attempts:       rec.Attempts,
		LastError:      rec.LastError,
	}
	if pk, ok := parseProviderIntentKey(rec.IdempotencyKey); ok {
		view.Identity = pk
	}
	return view
}

func emitFenceOp(v any, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "herd fence-op: encode output: %v\n", err)
			os.Exit(fenceOpExitError)
		}
		return
	}
	// Human summary carries the same facts; secrets are never in evidence.
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-op: encode output: %v\n", err)
		os.Exit(fenceOpExitError)
	}
	fmt.Println(string(b))
}
