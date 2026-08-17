package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/envelope"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/security"
)

// runControl is the production CLI for FAC-133 trusted control envelopes.
//
//	herd control issue  --task FAC-N --agent NAME [--worker SES] [--lease N] --body ...
//	herd control drain  --task FAC-N --agent NAME [--worker SES] [--lease N]
//	herd control classify <text>
//
// Worker session and lease are resolved from live Herdr + FAC-147 claim authority.
// Caller flags must match live state or be omitted.
func runControl() {
	runControlArgs(os.Args[2:])
}

func runControlArgs(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, controlUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "issue":
		runControlIssue(args[1:])
	case "drain":
		runControlDrain(args[1:])
	case "verify-sealed":
		runControlVerifySealed(args[1:])
	case "classify":
		runControlClassify(args[1:])
	case "-h", "--help", "help":
		fmt.Println(controlUsage)
	default:
		fmt.Fprintf(os.Stderr, "control: unknown mode %q\n%s\n", args[0], controlUsage)
		os.Exit(2)
	}
}

const controlUsage = `Usage:
  herd control issue  --task REF --agent NAME [--worker SESSION] [--lease N] --body TEXT [--packages csv] [--mail path]
  herd control drain  --task REF --agent NAME [--worker SESSION] [--lease N] [--mail path]
  herd control verify-sealed --file PATH
  herd control classify <free-form text>

Worker AgentSessionID and lease generation are resolved from live Herdr + FAC-147.
Optional --worker/--lease must match live state (invented values fail closed).

Env:
  HERD_CONTROL_SECRET   shared MAC secret (required for issue/drain)
  HERD_MAIL_FILE        canonical callback/control mailbox path (default .herd/control-mail.jsonl)
  HERD_CONTROL_ISSUER   issuer session id (default coordinator)
  HERD_CLAIMS_DB        optional override for FAC-147 lease DB`

func controlSecret() string {
	return strings.TrimSpace(os.Getenv("HERD_CONTROL_SECRET"))
}

func controlMailPath(override string) string {
	if override != "" {
		return override
	}
	if v := strings.TrimSpace(os.Getenv("HERD_MAIL_FILE")); v != "" {
		return v
	}
	return mail.CallbackMailPath(".")
}

func newControlPlane(mailPath string) (*dispatch.ControlPlane, error) {
	secret := controlSecret()
	if secret == "" {
		return nil, fmt.Errorf("HERD_CONTROL_SECRET is required (fail-closed)")
	}
	if err := os.MkdirAll(filepath.Dir(mailPath), 0o755); err != nil {
		return nil, err
	}
	issuer := strings.TrimSpace(os.Getenv("HERD_CONTROL_ISSUER"))
	if issuer == "" {
		issuer = "coordinator"
	}
	root := strings.TrimSpace(os.Getenv("HERD_ROOT"))
	if root == "" {
		root = "."
	}
	// Wire exact FAC-147 authority before any issue/drain.
	if err := security.WireCanonicalClaimAuthority(root); err != nil {
		return nil, err
	}
	cp := &dispatch.ControlPlane{
		Secret:            secret,
		Mailbox:           mail.NewMailbox(mailPath),
		IssuerRole:        envelope.RoleCoordinator,
		IssuerSession:     issuer,
		DurableRoot:       root,
		DurableIssuerPath: filepath.Join(root, ".herd", "control", "issuer-seq.json"),
		DeliverToAgent:    dispatch.DefaultHerdrDeliver,
		ClaimLookup:       security.ResolveClaimLookup(),
		RequireLiveBind:   true,
	}
	_ = dispatch.EnsureControlDurableDirs(root)
	return cp, nil
}

func runControlIssue(args []string) {
	fs := flag.NewFlagSet("control issue", flag.ContinueOnError)
	task := fs.String("task", "", "target task ref (FAC-N)")
	agent := fs.String("agent", "", "live Herdr agent name (preferred for AgentSessionID)")
	worker := fs.String("worker", "", "optional AgentSessionID — must match live if set")
	lease := fs.Int64("lease", 0, "optional lease generation — must match live if set (0=resolve)")
	body := fs.String("body", "", "scope correction body (may look like injection; MAC is trust)")
	packages := fs.String("packages", "", "comma-separated package allowlist")
	mailPath := fs.String("mail", "", "mailbox path override")
	note := fs.String("note", "", "optional scope note")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *task == "" || *body == "" {
		fmt.Fprintln(os.Stderr, "control issue: --task and --body are required")
		fmt.Fprintln(os.Stderr, controlUsage)
		os.Exit(2)
	}
	if *agent == "" && *worker == "" {
		fmt.Fprintln(os.Stderr, "control issue: --agent or --worker required")
		os.Exit(2)
	}
	cp, err := newControlPlane(controlMailPath(*mailPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "control issue: %v\n", err)
		os.Exit(1)
	}
	ws, liveLease, err := cp.ResolveLiveControlBinding(*task, *worker, *lease, *agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control issue: live bind failed: %v\n", err)
		os.Exit(1)
	}
	var pkgs []string
	if *packages != "" {
		for _, p := range strings.Split(*packages, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				pkgs = append(pkgs, p)
			}
		}
	}
	scope := &envelope.Scope{
		PackageAllowlist: pkgs,
		Exclusive:        true,
		Note:             *note,
	}
	// IssueAndEnforce: post → ApplyInboxControl → deliver verified decision only.
	ctrl, sess, applied, err := cp.IssueAndEnforce(ws, *task, liveLease, scope, *body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control issue: %v\n", err)
		os.Exit(1)
	}
	st, reason := sess.State()
	out := map[string]any{
		"envelope_id":   ctrl.ID,
		"sequence":      ctrl.Sequence,
		"task":          ctrl.TargetTask,
		"worker":        ctrl.TargetWorkerSession,
		"lease":         ctrl.LeaseGeneration,
		"session_state": st,
		"block_reason":  reason,
		"applied_count": len(applied),
		"verified":      true,
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func runControlDrain(args []string) {
	fs := flag.NewFlagSet("control drain", flag.ContinueOnError)
	task := fs.String("task", "", "target task ref")
	agent := fs.String("agent", "", "live Herdr agent name")
	worker := fs.String("worker", "", "optional AgentSessionID — must match live if set")
	lease := fs.Int64("lease", 0, "optional lease — must match live if set")
	mailPath := fs.String("mail", "", "mailbox path override")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *task == "" {
		fmt.Fprintln(os.Stderr, "control drain: --task required")
		os.Exit(2)
	}
	if *agent == "" && *worker == "" {
		fmt.Fprintln(os.Stderr, "control drain: --agent or --worker required")
		os.Exit(2)
	}
	cp, err := newControlPlane(controlMailPath(*mailPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "control drain: %v\n", err)
		os.Exit(1)
	}
	ws, liveLease, err := cp.ResolveLiveControlBinding(*task, *worker, *lease, *agent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control drain: live bind failed: %v\n", err)
		os.Exit(1)
	}
	sess, applied, err := cp.ApplyInboxControl(ws, *task, liveLease)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control drain: %v\n", err)
		os.Exit(1)
	}
	type row struct {
		MailID string              `json:"mail_id"`
		Status envelope.Status     `json:"status"`
		Trust  envelope.TrustClass `json:"trust"`
		Reason string              `json:"reason"`
		Error  string              `json:"error,omitempty"`
	}
	rows := make([]row, 0, len(applied))
	blocked := false
	for _, a := range applied {
		r := row{MailID: a.MailID}
		if a.Decision != nil {
			r.Status = a.Decision.Status
			r.Trust = a.Decision.Trust
			r.Reason = a.Decision.Reason
			if a.Decision.Status == envelope.StatusBlocked {
				blocked = true
			}
		}
		if a.Err != nil {
			r.Error = a.Err.Error()
		}
		rows = append(rows, r)
	}
	state, reason := sess.State()
	out := map[string]any{
		"applied":       rows,
		"session_state": state,
		"block_reason":  reason,
		"scope":         sess.CurrentScope(),
		"last_sequence": sess.LastSequence(),
		"worker":        ws,
		"lease":         liveLease,
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	if blocked || state == envelope.StateBlocked {
		os.Exit(1)
	}
	for _, a := range applied {
		if a.Err != nil {
			os.Exit(1)
		}
		if a.Decision != nil && (a.Decision.Status == envelope.StatusRejected || a.Decision.Status == envelope.StatusBlocked) {
			os.Exit(1)
		}
	}
}

func runControlVerifySealed(args []string) {
	fs := flag.NewFlagSet("control verify-sealed", flag.ContinueOnError)
	file := fs.String("file", "", "path to sealed control JSON")
	task := fs.String("task", "", "expected target task (or HERD_EXPECTED_TASK)")
	worker := fs.String("worker", "", "expected live AgentSessionID (or HERD_EXPECTED_WORKER)")
	lease := fs.Int64("lease", 0, "expected lease generation (or HERD_EXPECTED_LEASE)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "control verify-sealed: --file required")
		os.Exit(2)
	}
	if err := security.WorkerVerifySealedFile(*file, *task, *worker, *lease); err != nil {
		fmt.Fprintf(os.Stderr, "control verify-sealed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(`{"ok":true,"verified":true}`)
}

func runControlClassify(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "control classify: text required")
		os.Exit(2)
	}
	text := strings.Join(args, " ")
	trust := envelope.Classify(text)
	out := map[string]any{"trust": trust, "text_len": len(text)}
	if strings.TrimSpace(text) != "" && strings.TrimSpace(text)[0] == '{' {
		e, t, err := envelope.ParseUntrusted([]byte(text))
		out["parse_trust"] = t
		if err != nil {
			out["parse_error"] = err.Error()
		} else if e != nil {
			out["kind"] = e.Kind
			out["target_task"] = e.TargetTask
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
