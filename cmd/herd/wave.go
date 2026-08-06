package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runWave ports bin/herd-wave: the pre-wave checklist. It is deliberately a
// composition of existing subcommands rather than new logic — the value is
// running the whole checklist in one deterministic order so a wave never starts
// on unexamined state, not in reimplementing what each command already does.
//
// Every step is advisory: a failing check prints and continues, because a
// checklist that aborts halfway hides the rest of the picture. Only the
// mutating steps (--standing) can fail the command.
func runWave() {
	fs := flag.NewFlagSet("wave", flag.ExitOnError)
	standing := fs.Bool("standing", false, "Also raise and kick every standing lane")
	fs.Parse(os.Args[2:])

	self, err := os.Executable()
	if err != nil || strings.TrimSpace(self) == "" {
		self = "./bin/herd"
	}

	type step struct {
		title string
		args  []string
	}
	for _, s := range []step{
		{"route doctor", []string{"doctor-models"}},
		{"quota", []string{"quota"}},
		{"resources", []string{"resources"}},
		{"attention", []string{"attention"}},
		{"worktrees", []string{"worktrees"}},
		{"parked-work exposure", []string{"park", "audit"}},
		{"board drift", []string{"board-sync"}},
		{"next action", []string{"next"}},
	} {
		fmt.Printf("=== herd wave: %s ===\n", s.title)
		cmd := exec.Command(self, s.args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Advisory: report the failure, keep going. An early abort would hide
		// every remaining signal, which is the opposite of a pre-wave check.
		if err := cmd.Run(); err != nil {
			fmt.Printf("(herd %s reported a problem: %v)\n", strings.Join(s.args, " "), err)
		}
	}

	waveFailed := false
	if *standing {
		fmt.Println("=== herd wave: raising standing fleet ===")
		cmd := exec.Command(self, "standing")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "herd wave: standing raise failed: %v\n", err)
			waveFailed = true
		}
	}

	fmt.Println("=== herd wave: cheatsheet ===")
	for _, line := range []string{
		"herd next | herd attention | herd standing | herd up <lane>",
		"herd dispatch <REF> --lane smith | herd review --spawn",
		"herd harvest | herd unmerged --all | herd overlap",
		"herd stash push|pop  # worktree-scoped, never the shared stack",
		"herd park audit      # parked work one reset --hard from gone",
		"herd stop            # rest the fleet without destroying work",
		"herd claude-only|no-claude status  # durable routing posture",
	} {
		fmt.Printf("  %s\n", line)
	}

	if waveFailed {
		fmt.Fprintln(os.Stderr, "herd wave: complete with failures")
		os.Exit(1)
	}
	fmt.Println("herd wave: complete")
}
