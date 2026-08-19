package main

import (
	"fmt"
	"os"
	"strings"
)

// HERD_HELP_PROBE is an optional path. When set, markOperational appends the
// subcommand name so compiled help tests can prove zero side-effect entry.
const helpProbeEnv = "HERD_HELP_PROBE"

// isHelpArg reports whether s is the reserved help token -h or --help.
func isHelpArg(s string) bool {
	return s == "-h" || s == "--help"
}

// argsWantHelp reports whether args request help before an end-of-options
// delimiter. Tokens after "--" are literal payloads and never trigger help,
// so a ref equal to --help is accepted only as `herd dispatch -- --help` or
// `herd dispatch --ticket=--help`.
func argsWantHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpArg(a) {
			return true
		}
	}
	return false
}

// markOperational records that a subcommand entered operational code. Help
// paths must never call this. Tests set HERD_HELP_PROBE to observe entries.
func markOperational(command string) {
	path := os.Getenv(helpProbeEnv)
	if path == "" || command == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s\n", command)
	_ = f.Close()
}

// subcommandUsage is the reserved help text for each herd subcommand.
// Missing keys fall back to a generic line so the global help gate never
// reaches operational parsers for unknown-but-routed names.
var subcommandUsage = map[string]string{
	"control-surface":  "Usage: herd control-surface [--json]\n  Machine-readable discovery of public-agent operations only.",
	"board-freeze":     "Usage: herd board-freeze [status|on|off]\n  Durable provider-mutation freeze; on requires --actor and --reason.",
	"board-frozen":     "Usage: herd board-frozen\n  Exit 0 with the durable board-freeze trigger when mutations are frozen.",
	"broker":           "Usage: herd broker [ensure]\n  Serve or ensure the local broker runtime.",
	"claude-only":      "Usage: herd claude-only <on|off|status>\n  Legacy alias for herd posture claude-only.",
	"commands":         "Usage: herd commands [flags]\n  Inspect retained command sessions and run recovery sweeps.",
	"containers":       "Usage: herd containers [status|reconcile] [flags]\n  Inspect durable container lifecycle state and unowned containers.",
	"control":          "Usage: herd control <issue|drain> [flags]\n  Issue or drain authenticated control envelopes.",
	"init":             "Usage: herd init [--full]\n  Scaffold .herd/ config (optionally full 3-lane forge).",
	"clone":            "Usage: herd clone <repo-url> [target-dir]\n  Clone a repository and run herd init --full.",
	"preflight":        "Usage: herd preflight [--full-tree]\n  Workspace boundary, merge policy, and fleet readiness scanner.",
	"preflight-static": "Usage: herd preflight-static [--full-tree]\n  Workspace boundary, signal literal, and merge policy scanner.",
	"verify":           "Usage: herd verify [--build cmd] [--test cmd] [worktree]\n  Completion gate: real commits + build + tests.",
	"finish":           "Usage: herd finish <ref> --landed-sha <sha> [--receipt path] [--branch branch] [--worktree path]\n  Coordinator-only post-merge readiness gate; never mutates the board.",
	"selftest":         "Usage: herd selftest\n  Run self-test assertion suite against the active repository.",
	"status":           "Usage: herd status\n  Show herd workspace / daemon status.",
	"timeline":         "Usage: herd timeline [--file path] [--task ref] [--lane name] [--source name] [--type type]\n  Read chronological, filterable execution-event envelopes.",
	"pulse":            "Usage: herd pulse [--act [--spawn]] [--json] [--quiet] [--reason TEXT]\n  Coordinator heartbeat: observe by default; --act applies bounded renewals/callbacks.",
	"goal-guard":       "Usage: herd goal-guard (--set|--check|--clear) [flags]\n  Durable standing-lane continuation guard.",
	"wind-down":        "Usage: herd wind-down <on|off|status> [flags]\n  Control durable fleet launch posture.",
	"posture":          "Usage: herd posture <claude-only|no-claude|clear|status> [flags]\n  Durable provider-family execution policy.",
	"hold":             "Usage: herd hold <task> on|off|status --lane <name> --owner <role> [flags]\n  Generation-fenced lane/task hold.",
	"standing":         "Usage: herd standing [--dry-run|--status|--shutdown] [--only id,...] [id ...]\n  Raise, report, or shut down declarative standing control roles (not ephemeral workers).",
	"wave":             "Usage: herd wave [--standing|--up] [--json]\n  Pre-wave readiness report; optional standing raise after gates pass.",
	"daemon":           "Usage: herd daemon [flags]\n  Run the orchestration daemon.",
	"usage":            "Usage: herd usage\n  Show harness quota usage from OpenUsage CLI.",
	"quota":            "Usage: herd quota [flags]\n  Quota inspection helpers.",
	"review":           "Usage: herd review [ref] [--spawn|--pool]\n  Signed review admission, or warm-pool review surface dispatch when signer admission is unavailable.",
	"review-ledger":    "Usage: herd review-ledger list|queued|pending|tier <sha>|drift\n  Append-only review ledger operations; drift reports live standing builder-family mismatches.",
	"drain":            "Usage: herd drain [flags]\n  Drain control / review backlog.",
	"approve":          "Usage: herd approve [flags]\n  Approve a reviewed candidate.",
	"feedback":         "Usage: herd feedback [flags]\n  Census fleet-wide control-plane feedback.",
	"fence-broker":     "Usage: herd fence-broker [flags]\n  Inspect or enforce the broker authority fence.",
	"fence-provision":  "Usage: herd fence-provision [flags]\n  Provision the coordinator fence authority.",
	"harvest-merge":    "Usage: herd harvest-merge [flags]\n  Cherry-pick reviewed commits onto a fresh base.",
	"hostcreds":        "Usage: herd hostcreds <diagnose|session|selftest> [flags]\n  Query the host credentials oracle without launching OpenCode.",
	"labels":           "Usage: herd labels [flags]\n  Reconcile drifted Herdforge tab labels in place.",
	"merge-admit":      "Usage: herd merge-admit [flags]\n  Admit a reviewed candidate to the coordinator merge path.",
	"merge-complete":   "Usage: herd merge-complete [flags]\n  Record and validate completion of an admitted merge.",
	"netbroker-serve":  "Usage: herd netbroker-serve [flags]\n  Run the durable network allowlist broker process.",
	"no-claude":        "Usage: herd no-claude <on|off|status>\n  Legacy alias for herd posture no-claude.",
	"park":             "Usage: herd park park <slug> <sha> -m <message> | list [--json]\n  Make parked work durable and auditable.",
	"quota-supervisor": "Usage: herd quota-supervisor [flags]\n  Convert quota and process evidence into surface concurrency caps.",
	"receipt":          "Usage: herd receipt <issue|recover|release> [flags]\n  Issue, recover, or release signed task receipts.",
	"review-classify":  "Usage: herd review-classify <branch> [--tier R0|R1|R2|R3] [--pin SHA] [--json]\n  Classify candidate risk before review dispatch.",
	"review-ingest":    "Usage: herd review-ingest [flags]\n  Validate, admit, and audit reviewer verdict artifacts.",
	"role-inject":      "Usage: herd role-inject [flags]\n  Bind a session to its worker contract at session start.",
	"scope":            "Usage: herd scope [flags]\n  Publish the trusted task scope resolved by dispatch.",
	"seed-lane-state":  "Usage: herd seed-lane-state [flags]\n  Restore or seed lane state artifacts without overwriting existing state.",
	"signer-boundary":  "Usage: herd signer-boundary <serve|establish|status|prove|sign> [flags]\n  Operate the OS signing boundary.",
	"spin":             "Usage: herd spin [flags]\n  Detect stalled or spinning agent panes.",
	"stash":            "Usage: herd stash push [-m <msg>] [-- <paths>...] | pop | apply | list\n  Use a worktree-scoped private stash namespace.",
	"stop":             "Usage: herd stop [flags]\n  Stop the herd without deleting worktrees; dry-run by default.",
	"task":             "Usage: herd task <get|comment|verdict> [flags]\n  Access the receipt-gated task broker.",
	"verify-fac151":    "Usage: herd verify-fac151 [flags]\n  Run the fixed hermetic FAC-151 verifier profile.",
	"watch":            "Usage: herd watch [--stream] [flags]\n  Fire when an agent settles and optionally feed harvest triggers.",
	"board-done": "Usage: herd board-done <ref> [--receipt <path>] [--override-policy <p> --override-actor <who> --override-reason <why> --override-evidence <what>]\n" +
		"  Close a card from its task-bound completion receipt, or by an attributable manual override.",
	"board-audit": "Usage: herd board-audit [--json]\n  Report Done cards no completion receipt closed. Read-only; never mutates the board.",
	"board-sync":  "Usage: herd board-sync [flags]\n  Reconcile board status against git reality and live lanes (report only).\n  --fix: advance to-do cards to in-progress when a live lane or branch proves work is in flight.",
	"sh":          "Usage: herd sh\n  Interactive REPL shell (alias: herd repl).",
	"repl":        "Usage: herd repl\n  Interactive REPL shell (alias: herd sh).",
	"send":        "Usage: herd send <pane|name> \"<text>\" [--file path] [--no-verify] [--timeout s]\n  Deliver a prompt and verify consumption. Prefer herdr-deliver for durable digests.",
	"herdr-deliver": `Usage: herd herdr-deliver --key <op> --generation <n> --target <name> [--session <id>] [--file path] [--wait] [--timeout s] [--state path]
  Durably deliver exact prompt bytes from stdin or --file to one Herdr session.
  Positional free-form text is rejected (FAC-183 shell-literal incident class).`,
	"cleanup":         "Usage: herd cleanup [flags]\n  One-shot tab / worktree cleanup sweep.",
	"forge":           "Usage: herd forge [--loop] [--retry-approve <ref>] [flags]\n  Forge orchestration entrypoints. Legacy receiptless approvals are suppressed durably; --retry-approve explicitly re-attempts one.",
	"legacy-receipts": "Usage: herd legacy-receipts [--json] [--tombstone <ref> --reason <text>]\n  Audit or tombstone receiptless legacy in-progress tasks (fail-closed).",
	"up":              "Usage: herd up <lane-name>\n  Bring up a configured lane.",
	"activate":        "Usage: herd activate [flags]\n  Activate fleet / lane posture.",
	"validate-config": "Usage: herd validate-config [path]\n  Validate .herd/herd.yaml.",
	"doctor-models":   "Usage: herd doctor-models [flags]\n  Probe lane models for quota / availability.",
	"tool-probe":      "Usage: herd tool-probe <model>\n  Exit 0 only if the model executes a tool.",
	"shoot":           "Usage: herd shoot <pane|name> <refocus message>\n  Interrupt a stalled agent and refocus it.",
	"shot": `Usage: herd shot <task-ref> [flags]        (bounded ONE-TASK lane, FAC-89)
       herd shot <prompt words>            (headless prompt through the quota router)

Task lane — eligibility, atomic claim, isolated dispatch, completion callback,
exact-SHA verification, handoff to review. It never merges and never marks a
card Done. The ref must come FIRST.

Flags:
  --lane <name>     Lane name or role to dispatch into (default: worker)
  --timeout <s>     Seconds to wait for the completion callback (default: 900)
  --risk <R0-R3>    Risk tier when the board carries no risk label
  --json            Emit the evidence packet as JSON on stdout

Builder half of the loop (run from the task worktree):
  herd shot <task-ref> --report complete --sha <40-hex> --lease <n>
  herd shot <task-ref> --report blocked --detail "<why>" --lease <n>

Prompt lane flags: --task <shape> --provider <name> --schema <file>
                   --timeout <s> --dry-run`,
	"next": "Usage: herd next [--role <role>] [--lane <lane>]\n  Deterministic next-task selection scoped to claimable candidates.",
	"dispatch": `Usage: herd dispatch <ticket-ref> [flags]
  Dispatch a ticket to a worktree and launch an agent.

Flags:
  --no-launch       Create worktree and packet only, no agent
  --lane <name>     Lane name from config (default: worker)
	  --environment-plan <id> Exact operator-granted environment plan ID (required in production)
  --ticket <ref>    Ticket ref when the value begins with '-' (or use -- <ref>)

Help is reserved: -h / --help never become the ticket. A literal payload equal
to --help is accepted only as:
  herd dispatch -- --help
	  herd dispatch --ticket=--help

Recovery:
  herd dispatch cancel <ticket-ref> --lease <generation>
  Releases only that coordinator-dispatch lease generation.`,
	"envplan": `Usage: herd envplan <create|inspect|grant|revoke> [flags]
  Operator-managed, repository-relative environment capability plans. Plans
  record capability evidence and bindings only; credential values are refused.`,
	"deps": `Usage: herd deps <selftest|check|reconcile|migrate> [args]
  Packet↔board dependency-graph conformance (FAC-159).

Commands:
  selftest           Hermetic drift fixtures + mutation controls
  check <ref>        Validate launch eligibility without side effects
  reconcile <ref>    Print stable JSON reconcile report
  migrate            Dry-run description fence; --apply is coordinator-only`,
	"tests-for": `Usage: herd tests-for [--graph-tool path] [--no-rebuild] [--timeout d] <base>..<candidate>
  Deterministic targeted verification plan for an exact revision pair (FAC-160).
  Flags precede the range; pass -- before a ref that begins with '-'.

  Graph-derived absence (not_found / zero callers / zero tests) is trusted only
  when the local code-review-graph index is bound to the candidate commit AND
  covers the tracked-source manifest. Incomplete parity triggers one bounded
  full rebuild; if parity still fails the run exits non-zero (BLOCKED) and the
  emitted plan broadens to the full profile instead of narrowing.`,
	"harvest":    "Usage: herd harvest [--quiet] [--json]\n  Fleet-wide worktree harvest sweep.",
	"unmerged":   "Usage: herd unmerged [--all|<worktree>]\n  List unmerged commits in worktrees.",
	"lost":       "Usage: herd lost [flags]\n  Find lost / orphaned work.",
	"throughput": "Usage: herd throughput [flags]\n  Throughput metrics.",
	"worktrees": `herd-worktrees , one-shot collision snapshot across every repo worktree:
  {worktree, branch, ahead-of-origin/main commits, dirty files, touched files}.
  Replaces the coordinator's manual per-worktree ` + "`git log` + `git status`" + `
  loop when ranking claimable tickets (planner request 2026-07-21). A final
  COLLISIONS section lists any file touched (committed-ahead or dirty) by
  more than one worktree, so collision-checking is exact, not eyeballed.

    herd worktrees            # human summary + collisions
    herd worktrees --json     # machine-readable array
    herd worktrees --files    # also list per-worktree touched files`,
	"overlap": "Usage: herd overlap [flags]\n  Overlap / collision analysis.",
	"rescue": "Usage: herd rescue [<pane-id>] [--empty-siblings] [--workspace ID] [--label NAME] [--apply] [--json]\n" +
		"  Diagnose cramped/split agent panes. Dry-run by default; --apply repairs one proven target.",
	"attention":    "Usage: herd attention [flags]\n  Coordinator-eyes triage for the standing fleet.",
	"process":      "Usage: herd process [flags]\n  Process-engine inspection.",
	"resolve-lane": "Usage: herd resolve-lane [flags]\n  Resolve canonical lane identity.",
	"route":        "Usage: herd route [flags]\n  Model / surface routing helpers.",
	"kick":         "Usage: herd kick [flags]\n  Nudge a stalled lane / agent; --cadence throttles repeat kicks and --repair bypasses freeze.",
	"lifecycle":    "Usage: herd lifecycle [flags]\n  Lifecycle inspection.",
	"resources":    "Usage: herd resources [flags]\n  Resource inventory.",
	"lock":         "Usage: herd lock <acquire|release|status|with> [flags]\n  Shared-checkout dir lock.",
	"slot":         "Usage: herd slot <acquire|release|status|with> [flags]\n  Machine-wide heavy-phase semaphore.",
	"mail": `Usage:
  herd mail send --from NAME --to RECIPIENT (--body TEXT | --file path | stdin) [--subject TEXT] [--mail path]
  herd mail inbox --recipient NAME [--mail path]
  herd mail read --recipient NAME [--mail path]
  herd mail control <issue|drain> [flags]

Ordinary durable messages use the local mailbox and are not authenticated control.
The body may be supplied byte-for-byte with --file, --file -, or stdin; do not combine payload sources.
Privileged authenticated control callbacks/envelopes are available only through
herd mail control, which delegates to the existing herd control issue/drain
validation, generation fences, deduplication, and delivery path.`,
	// Byte-stable contract with reset_safe_cli_test.go (exact single line).
	"reset-safe": resetSafeUsage,
	"fresh-build": "Usage: herd fresh-build <pkg-or-path> [--dry-run]\n" +
		"  Prove whether a cross-package type/build error is REAL or just stale dist.\n" +
		"  Resolves the dependency-plus-self chain, clears only that chain's build\n" +
		"  artifacts, rebuilds fresh, and prints a one-line verdict (profile-driven;\n" +
		"  pnpm is one adapter).",
	"command": "Usage: herd command <authorize|run|receipts> [flags] -- <argv>\n" +
		"  Root/coordinator command authorization boundary (FAC-195).\n" +
		"  A guarded command runs ONLY through `herd command run`, which durably\n" +
		"  consumes one attempt before creating any process and refuses (exit 77)\n" +
		"  once the budget is spent or a stop-on-first-failure command has failed.\n" +
		"\n" +
		"    herd command authorize --id C1 --lane worker-a --session S --authority root \\\n" +
		"        --max-attempts 1 --disposition stop-on-first-failure -- go test ./pkg/x\n" +
		"    herd command run --id C1 --lane worker-a --session S -- go test ./pkg/x\n" +
		"    herd command receipts --id C1 [--json]",
}

func usageFor(command string) string {
	if u, ok := subcommandUsage[command]; ok {
		return u
	}
	return fmt.Sprintf("Usage: herd %s [args]\n  No detailed help is registered for this command.", command)
}

// exitIfHelp prints subcommand usage and exits 0 when args request help.
// Returns false when the caller should continue into operational code.
func exitIfHelp(command string, args []string) bool {
	if !argsWantHelp(args) {
		return false
	}
	fmt.Println(usageFor(command))
	os.Exit(0)
	return true
}

// parseTicketRef resolves a dispatch/ticket-style positional.
// Priority: --ticket flag (including equals form), else first positional
// collected after FlagSet parsing. Dash-prefixed positionals are only
// reachable after the end-of-options delimiter `--` (flag.Parse would
// otherwise reject them as unknown flags) or via --ticket=.
// Bare -h/--help never reaches positionals: the global help gate and
// FlagSet ErrHelp consume them first.
func parseTicketRef(ticketFlag string, positionals []string) (string, error) {
	if strings.TrimSpace(ticketFlag) != "" {
		return ticketFlag, nil
	}
	if len(positionals) == 0 {
		return "", fmt.Errorf("missing ticket ref")
	}
	return positionals[0], nil
}
