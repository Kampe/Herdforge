package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestHarvestMergeCLIPositionalBranchContract(t *testing.T) {
	binary := buildHerd(t)

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantOutput string
		reject     string
	}{
		{
			name: "documented verify-landed command",
			args: []string{
				"harvest-merge", "fac-629-r2", "--verify-landed", "--ref", "FAC-629",
				"--candidate", "21924c3bef05d459cf1ef38bdbabdf412a6fdec4", "--pr", "666",
			},
			wantExit:   1,
			wantOutput: "no worktree found for branch fac-629-r2",
			reject:     "usage:",
		},
		{
			name:       "positional and flag mismatch",
			args:       []string{"harvest-merge", "lane-a", "--branch", "lane-b", "--verify-landed", "--ref", "FAC-658"},
			wantExit:   2,
			wantOutput: "positional branch lane-a disagrees with --branch lane-b",
		},
		{
			name:       "missing branch identity",
			args:       []string{"harvest-merge", "--verify-landed", "--ref", "FAC-658"},
			wantExit:   2,
			wantOutput: "branch identity is required for --verify-landed",
		},
		{
			name:     "ordinary harvest keeps lane and branch syntax",
			args:     []string{"harvest-merge", "lane-a", "--branch", "lane-b", "--title", "bounded harvest", "--dry-run"},
			wantExit: 1,
			reject:   "positional branch lane-a disagrees with --branch lane-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binary, tt.args...)
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if got := commandExitCode(err); got != tt.wantExit {
				t.Fatalf("exit=%d, want %d; err=%v\n%s", got, tt.wantExit, err, out)
			}
			if tt.wantOutput != "" && !strings.Contains(string(out), tt.wantOutput) {
				t.Fatalf("output must contain %q:\n%s", tt.wantOutput, out)
			}
			if tt.reject != "" && strings.Contains(string(out), tt.reject) {
				t.Fatalf("output must not contain %q:\n%s", tt.reject, out)
			}
		})
	}
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
