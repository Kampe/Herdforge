package tui

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type REPL struct {
	In  io.Reader
	Out io.Writer
}

func NewREPL(in io.Reader, out io.Writer) *REPL {
	return &REPL{
		In:  in,
		Out: out,
	}
}

func ParseREPLCommand(input string) (string, []string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", nil
	}
	parts := strings.Fields(trimmed)
	return strings.ToLower(parts[0]), parts[1:]
}

func (r *REPL) Eval(cmd string, args []string) (string, bool) {
	switch cmd {
	case "help":
		return "Available commands: status, lanes, budget, claim <ref>, exit", false
	case "status":
		return "Herdforge Daemon Status: ACTIVE | Lanes: 4 | Memory: 23MB", false
	case "lanes":
		return "Active Lanes: lane-1 (claude-3-5-sonnet), lane-2 (gpt-4o)", false
	case "budget":
		return "Current Budget: $0.045 / $10.00 USD (0.45%)", false
	case "claim":
		if len(args) == 0 {
			return "Error: missing task ref. Usage: claim <ref>", false
		}
		return fmt.Sprintf("Task %s claimed successfully", args[0]), false
	case "exit", "quit":
		return "Goodbye!", true
	default:
		return fmt.Sprintf("Unknown command: %s. Type 'help' for commands.", cmd), false
	}
}

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
