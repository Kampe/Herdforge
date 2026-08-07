package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/verifier"
)

func runFAC151Hermetic() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: herd verify-fac151")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := verifier.RunFAC151Hermetic(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-fac151 failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("verify-fac151: container=%s exit=%d output=%s removed=%t\n", result.ContainerID, result.ExitCode, result.OutputDigest, result.Removed)
}
