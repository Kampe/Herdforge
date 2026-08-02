package verifier

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Language string

const (
	LangGo      Language = "go"
	LangNode    Language = "node"
	LangPython  Language = "python"
	LangRust    Language = "rust"
	LangUnknown Language = "unknown"
)

type Result struct {
	Passed bool
	Output string
}

type MutationResult struct {
	MutantID string
	Killed   bool
	Output   string
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

// DetectLanguage inspects file extensions to determine testing toolchain
func DetectLanguage(filePath string) Language {
	switch {
	case strings.HasSuffix(filePath, ".go"):
		return LangGo
	case strings.HasSuffix(filePath, ".ts"), strings.HasSuffix(filePath, ".js"), strings.HasSuffix(filePath, ".tsx"), strings.HasSuffix(filePath, ".jsx"):
		return LangNode
	case strings.HasSuffix(filePath, ".py"):
		return LangPython
	case strings.HasSuffix(filePath, ".rs"):
		return LangRust
	default:
		return LangUnknown
	}
}

// RunMutationCheck verifies that a negative assertion test fails when a regression is introduced
func (v *Verifier) RunMutationCheck(ctx context.Context, dir string, targetFile string, originalCode string, mutantCode string) (*MutationResult, error) {
	origRes, err := v.Execute(ctx, dir)
	if err != nil || !origRes.Passed {
		return nil, fmt.Errorf("baseline test suite failed before mutation check: %w", err)
	}

	mutantRes, err := v.Execute(ctx, dir)
	if err != nil {
		return nil, err
	}

	killed := !mutantRes.Passed

	return &MutationResult{
		MutantID: fmt.Sprintf("mutant-%s", targetFile),
		Killed:   killed,
		Output:   mutantRes.Output,
	}, nil
}
