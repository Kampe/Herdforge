package main

import (
	"fmt"
	"os"
	"sort"
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
	"init":          "Usage: herd init [--full]\n  Scaffold .herd/ config (optionally full 3-lane forge).",
	"clone":         "Usage: herd clone <repo-url> [target-dir]\n  Clone a repository and run herd init --full.",
	"preflight":     "Usage: herd preflight\n  Workspace boundary and absolute-path leak scanner.",
	"verify":        "Usage: herd verify [--build cmd] [--test cmd] [worktree]\n  Completion gate: real commits + build + tests.",
	"selftest":      "Usage: herd selftest\n  Run self-test assertion suite against the active repository.",
	"status":        "Usage: herd status\n  Show herd workspace / daemon status.",
	"pulse":         "Usage: herd pulse [--act [--spawn]] [--json] [--quiet] [--reason TEXT]\n  Coordinator heartbeat: observe by default; --act applies bounded renewals/callbacks.",
	"wind-down":     "Usage: herd wind-down <on|off|status> [flags]\n  Control durable fleet launch posture.",
	"hold":          "Usage: herd hold <task> on|off|status --lane <name> --owner <role> [flags]\n  Generation-fenced lane/task hold.",
	"standing": "Usage: herd standing [--dry-run|--status|--shutdown] [--only id,...] [id ...]\n  Raise, report, or shut down declarative standing control roles (not ephemeral workers).",
	"daemon":        "Usage: herd daemon [flags]\n  Run the orchestration daemon.",
	"usage":         "Usage: herd usage\n  Show harness quota usage from OpenUsage CLI.",
	"quota":         "Usage: herd quota [flags]\n  Quota inspection helpers.",
	"review":        "Usage: herd review [ref] [--spawn]\n  Adversarial review pipeline for in-progress work.",
	"review-ledger": "Usage: herd review-ledger [flags]\n  Append-only review ledger operations.",
	"drain":         "Usage: herd drain [flags]\n  Drain control / review backlog.",
	"approve":       "Usage: herd approve [flags]\n  Approve a reviewed candidate.",
	"board-done":    "Usage: herd board-done <ref> [--evidence <sha>] [--force]\n  Mark board card done with evidence.",
	"board-sync":    "Usage: herd board-sync [flags]\n  Reconcile multi-board state.",
	"sh":            "Usage: herd sh\n  Interactive REPL shell (alias: herd repl).",
	"repl":          "Usage: herd repl\n  Interactive REPL shell (alias: herd sh).",
	"send":          "Usage: herd send <pane|name> \"<text>\" [--file path] [--no-verify] [--timeout s]\n  Deliver a prompt and verify consumption. Prefer herdr-deliver for durable digests.",
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
	"next":            "Usage: herd next [flags]\n  Deterministic next-task selection.",
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
	"overlap":      "Usage: herd overlap [flags]\n  Overlap / collision analysis.",
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
}

// knownSubcommands is the deterministic set of routed herd subcommands.
// Used by the global help gate and table-driven help tests.
func knownSubcommands() []string {
	names := make([]string, 0, len(subcommandUsage))
	for name := range subcommandUsage {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
