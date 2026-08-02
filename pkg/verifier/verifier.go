package verifier

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Result struct {
	Passed bool
	Output string
}

type Verifier struct {
	Command string
}

func NewVerifier(command string) *Verifier {
	return &Verifier{Command: command}
}

func (v *Verifier) Execute(ctx context.Context, dir string) (*Result, error) {
	if v.Command == "" {
		return &Result{Passed: true, Output: "no verification command specified"}, nil
	}

	parts := strings.Fields(v.Command)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return &Result{
			Passed: false,
			Output: fmt.Sprintf("Verification failed: %v\nOutput:\n%s", err, string(output)),
		}, nil
	}

	return &Result{
		Passed: true,
		Output: string(output),
	}, nil
}
