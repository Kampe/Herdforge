package herdr

import (
	"fmt"
	"testing"
)

// PaneProcessInfo returns whatever herdr reported. PaneProcessArgv is the one
// that guarantees argv, because a reader that treats argv as evidence must not
// silently receive processes that have none.
func TestPaneProcessArgvFillsArgvHerdrOmitted(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"foreground_processes":[
			{"pid":101,"name":"pi","cwd":"/w"},
			{"pid":102,"name":"claude","cwd":"/w","argv":["claude","--model","claude-sonnet-5"]}
		]}}}`, nil
	}
	defer SetPIDArgvReader(func(pid int) ([]string, error) {
		if pid == 101 {
			return []string{"pi", "--model", "anthropic/claude-fable-5", "--thinking", "high"}, nil
		}
		return nil, fmt.Errorf("unexpected pid %d", pid)
	})()

	// The raw accessor leaves the gap exactly where it was.
	raw, err := PaneProcessInfo("pane-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw[0].Argv) != 0 {
		t.Fatalf("PaneProcessInfo hydrated argv it should have left alone: %v", raw[0].Argv)
	}

	procs, readErrs := PaneProcessArgv("pane-1")
	if len(readErrs) != 0 {
		t.Fatalf("unexpected read errors: %v", readErrs)
	}
	if len(procs) != 2 {
		t.Fatalf("got %d processes, want 2", len(procs))
	}
	if len(procs[0].Argv) == 0 || procs[0].Argv[2] != "anthropic/claude-fable-5" {
		t.Fatalf("pid 101 argv = %v, want it filled from the OS", procs[0].Argv)
	}
	// An argv herdr already reported is not re-read or overwritten.
	if procs[1].Argv[2] != "claude-sonnet-5" {
		t.Fatalf("pid 102 argv = %v, want it left as reported", procs[1].Argv)
	}
}

// A process that exited between inventory and read is ordinary; the survivors
// must still come back, with the gap reported rather than hidden.
func TestPaneProcessArgvReportsPerPIDFailuresWithoutDroppingSurvivors(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"foreground_processes":[
			{"pid":201,"name":"gone","cwd":"/w"},
			{"pid":202,"name":"claude","cwd":"/w","argv":["claude","--model","claude-sonnet-5"]}
		]}}}`, nil
	}
	defer SetPIDArgvReader(func(pid int) ([]string, error) {
		return nil, fmt.Errorf("no such process %d", pid)
	})()

	procs, readErrs := PaneProcessArgv("pane-1")
	if len(procs) != 2 {
		t.Fatalf("a failed argv read dropped a process: got %d, want 2", len(procs))
	}
	if len(readErrs) != 1 {
		t.Fatalf("read errors = %v, want the one failure surfaced", readErrs)
	}
	if len(procs[0].Argv) != 0 {
		t.Fatalf("pid 201 argv = %v, want it left empty", procs[0].Argv)
	}
	if procs[1].Argv[2] != "claude-sonnet-5" {
		t.Fatalf("survivor argv = %v", procs[1].Argv)
	}
}

func TestPaneProcessArgvPropagatesInventoryFailure(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) { return "", fmt.Errorf("herdr down") }

	procs, errs := PaneProcessArgv("pane-1")
	if procs != nil || len(errs) != 1 {
		t.Fatalf("procs=%v errs=%v, want no processes and the failure reported", procs, errs)
	}
}
