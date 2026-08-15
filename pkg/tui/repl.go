package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// REPL is an interactive read-eval-print loop for herd fleet operations.
// FAC-300: Eval delegates to narrow Backend interfaces instead of fabricating
// output. When Backend is nil or its interfaces are absent, Eval returns
// explicitly labeled offline (read-only) messages — never fake success.
type REPL struct {
	In      io.Reader
	Out     io.Writer
	Backend *Backend
}

// NewREPL creates a REPL with optional backend. Pass nil for offline
// read-only mode.
func NewREPL(in io.Reader, out io.Writer, backend *Backend) *REPL {
	return &REPL{
		In:      in,
		Out:     out,
		Backend: backend,
	}
}

// ParseREPLCommand splits a raw input line into a lowercase command and its
// remaining args. This parser seam is preserved unchanged (FAC-300).
func ParseREPLCommand(input string) (string, []string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", nil
	}
	parts := strings.Fields(trimmed)
	return strings.ToLower(parts[0]), parts[1:]
}

// Eval resolves a single command to an output line and a shouldExit flag.
// FAC-300: all live-data commands consult Backend; nil/absent interfaces
// produce labeled offline messages, not fabricated success.
func (r *REPL) Eval(cmd string, args []string) (string, bool) {
	switch cmd {
	case "help":
		return "Available commands: status, lanes, budget, claim <ref>, tasks, exit", false
	case "status":
		return r.evalStatus(), false
	case "lanes":
		return r.evalLanes(), false
	case "budget":
		return r.evalBudget(), false
	case "tasks":
		return r.evalTasks(), false
	case "claim":
		if len(args) == 0 {
			return "Error: missing task ref. Usage: claim <ref>", false
		}
		return r.evalClaim(args[0]), false
	case "exit", "quit":
		return "Goodbye!", true
	default:
		return fmt.Sprintf("Unknown command: %s. Type 'help' for commands.", cmd), false
	}
}

func (r *REPL) evalStatus() string {
	b := r.Backend
	if b == nil || b.Status == nil {
		return "[OFFLINE] status: no live daemon connected (read-only mode)"
	}
	ds, err := b.Status.QueryStatus()
	if err != nil {
		return fmt.Sprintf("status: ERROR — %v", err)
	}
	lastErr := ds.LastError
	if lastErr == "" {
		lastErr = "(none)"
	}
	return fmt.Sprintf("Daemon Status: %s | Last Error: %s | Updated: %s",
		ds.State, lastErr, ds.UpdatedAt.Format("2006-01-02 15:04:05"))
}

func (r *REPL) evalLanes() string {
	b := r.Backend
	if b == nil || b.Fleet == nil {
		return "[OFFLINE] lanes: no live fleet connection (read-only mode)"
	}
	lanes := b.Fleet.Lanes()
	if len(lanes) == 0 {
		return "Configured Lanes: (none)"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Configured Lanes: %d\n", len(lanes)))
	for _, l := range lanes {
		tag := "ephemeral"
		if l.Standing {
			tag = "standing"
		}
		sb.WriteString(fmt.Sprintf("  %s — role=%s model=%s [%s]\n", l.Name, l.Role, l.Model, tag))
	}
	label, err := b.Fleet.FleetStatus()
	if err != nil {
		sb.WriteString(fmt.Sprintf("Fleet Status: ERROR — %v\n", err))
	} else {
		sb.WriteString(fmt.Sprintf("Fleet Status: %s\n", label))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (r *REPL) evalBudget() string {
	b := r.Backend
	if b == nil || b.Budget == nil {
		return "[OFFLINE] budget: no live budget manager connected (read-only mode)"
	}
	bs, err := b.Budget.QueryBudget()
	if err != nil {
		return fmt.Sprintf("budget: ERROR — %v", err)
	}
	pct := 0.0
	if bs.MaxUSD > 0 {
		pct = (bs.SpentUSD / bs.MaxUSD) * 100
	}
	tag := ""
	if bs.Exhausted {
		tag = " [EXHAUSTED]"
	}
	return fmt.Sprintf("Budget: $%.4f / $%.4f USD (%.2f%%)%s | Tokens: %d",
		bs.SpentUSD, bs.MaxUSD, pct, tag, bs.Tokens)
}

func (r *REPL) evalTasks() string {
	b := r.Backend
	if b == nil || b.Tasks == nil {
		return "[OFFLINE] tasks: no live provider connected (read-only mode)"
	}
	ctx := context.Background()
	status := "to-do"
	tasks, err := b.Tasks.ListTasks(ctx, status)
	if err != nil {
		return fmt.Sprintf("tasks: ERROR — %v", err)
	}
	if len(tasks) == 0 {
		return "Task Queue: (no pending tasks)"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task Queue: %d pending\n", len(tasks)))
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("  %s | %s | %s | %s\n", t.Ref, t.Priority, t.Status, t.Title))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (r *REPL) evalClaim(ref string) string {
	b := r.Backend
	if b == nil || b.Claims == nil {
		return fmt.Sprintf("[OFFLINE] claim %s: no live claim manager connected (read-only mode)", ref)
	}
	ctx := context.Background()
	claimed, err := b.Claims.IsClaimed(ctx, ref)
	if err != nil {
		return fmt.Sprintf("claim %s: ERROR — %v", ref, err)
	}
	if claimed {
		claims, listErr := b.Claims.ActiveClaims(ctx)
		if listErr != nil {
			return fmt.Sprintf("claim %s: CLAIMED (active claims query failed: %v)", ref, listErr)
		}
		for _, c := range claims {
			if c.Ref == ref {
				return fmt.Sprintf("claim %s: CLAIMED by %s (role=%s, status=%s, expires=%s)",
					ref, c.OwnerID, c.Role, c.Status, c.ExpiresAt.Format("2006-01-02 15:04:05"))
			}
		}
		return fmt.Sprintf("claim %s: CLAIMED (no active lease detail found)", ref)
	}
	return fmt.Sprintf("claim %s: not claimed (available)", ref)
}

// Run is the read-loop driver. This seam is preserved unchanged (FAC-300).
func (r *REPL) Run() error {
	scanner := bufio.NewScanner(r.In)
	_, _ = fmt.Fprint(r.Out, "herd> ")

	for scanner.Scan() {
		line := scanner.Text()
		cmd, args := ParseREPLCommand(line)
		if cmd == "" {
			_, _ = fmt.Fprint(r.Out, "herd> ")
			continue
		}

		output, shouldExit := r.Eval(cmd, args)
		_, _ = fmt.Fprintln(r.Out, output)

		if shouldExit {
			break
		}
		_, _ = fmt.Fprint(r.Out, "herd> ")
	}

	return scanner.Err()
}
