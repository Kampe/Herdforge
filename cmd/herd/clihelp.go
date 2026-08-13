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
	"control-surface": "Usage: herd control-surface [--json]\n  Machine-readable discovery of public-agent operations only.",
	"init":            "Usage: herd init [--full]\n  Scaffold .herd/ config (optionally full 3-lane forge).",
	"clone":           "Usage: herd clone <repo-url> [target-dir]\n  Clone a repository and run herd init --full.",
	"preflight":       "Usage: herd preflight\n  Workspace boundary and absolute-path leak scanner.",
	"verify":          "Usage: herd verify [--build cmd] [--test cmd] [worktree]\n  Completion gate: real commits + build + tests.",
	"selftest":        "Usage: herd selftest\n  Run self-test assertion suite against the active repository.",
	"status":          "Usage: herd status\n  Show herd workspace / daemon status.",
	"timeline":        "Usage: herd timeline [--file path] [--task ref] [--lane name] [--source name] [--type type]\n  Read chronological, filterable execution-event envelopes.",
	"pulse":           "Usage: herd pulse [--act [--spawn]] [--json] [--quiet] [--reason TEXT]\n  Coordinator heartbeat: observe by default; --act applies bounded renewals/callbacks.",
	"wind-down":       "Usage: herd wind-down <on|off|status> [flags]\n  Control durable fleet launch posture.",
	"posture":         "Usage: herd posture <claude-only|no-claude|clear|status> [flags]\n  Durable provider-family execution policy.",
	"hold":            "Usage: herd hold <task> on|off|status --lane <name> --owner <role> [flags]\n  Generation-fenced lane/task hold.",
	"standing":        "Usage: herd standing [--dry-run|--status|--shutdown] [--only id,...] [id ...]\n  Raise, report, or shut down declarative standing control roles (not ephemeral workers).",
	"wave":            "Usage: herd wave [--standing|--up] [--json]\n  Pre-wave readiness report; optional standing raise after gates pass.",
	"daemon":          "Usage: herd daemon [flags]\n  Run the orchestration daemon.",
	"usage":           "Usage: herd usage\n  Show harness quota usage from OpenUsage CLI.",
	"quota":           "Usage: herd quota [flags]\n  Quota inspection helpers.",
	"review":          "Usage: herd review [ref] [--spawn]\n  Adversarial review pipeline for in-progress work.",
	"review-ledger":   "Usage: herd review-ledger [flags]\n  Append-only review ledger operations.",
	"drain":           "Usage: herd drain [flags]\n  Drain control / review backlog.",
	"approve":         "Usage: herd approve [flags]\n  Approve a reviewed candidate.",
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
	"forge":           "Usage: herd forge [--loop] [flags]\n  Forge orchestration entrypoints.",
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
	"next": "Usage: herd next [flags]\n  Deterministic next-task selection.",
	"dispatch": `Usage: herd dispatch <ticket-ref> [flags]
  Dispatch a ticket to a worktree and launch an agent.

Flags:
  --no-launch       Create worktree and packet only, no agent
  --lane <name>     Lane name from config (default: worker)
  --ticket <ref>    Ticket ref when the value begins with '-' (or use -- <ref>)

Help is reserved: -h / --help never become the ticket. A literal payload equal
to --help is accepted only as:
  herd dispatch -- --help
  herd dispatch --ticket=--help`,
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
	"kick":         "Usage: herd kick [flags]\n  Nudge a stalled lane / agent.",
	"lifecycle":    "Usage: herd lifecycle [flags]\n  Lifecycle inspection.",
	"resources":    "Usage: herd resources [flags]\n  Resource inventory.",
	"lock":         "Usage: herd lock <acquire|release|status|with> [flags]\n  Shared-checkout dir lock.",
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
	return fmt.Sprintf("Usage: herd %s [args]\n  Run 'herd --help' for the command list.", command)
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
