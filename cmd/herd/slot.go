package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/slot"
)

const slotUsage = "Usage: herd slot acquire|release|status|with [flags] -- <cmd...>"

func runSlot() {
	args := os.Args[2:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(slotUsage)
		return
	}
	s, err := slot.Default()
	if err != nil {
		fmt.Fprintln(os.Stderr, "herd slot:", err)
		os.Exit(1)
	}
	switch args[0] {
	case "status":
		enc := json.NewEncoder(os.Stdout)
		if err := enc.Encode(s.Status()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "acquire":
		fs := flag.NewFlagSet("slot acquire", flag.ContinueOnError)
		purpose := fs.String("purpose", "manual", "phase purpose")
		wait := fs.Duration("wait", slot.DefaultTimeout, "maximum wait")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		lease, err := s.Acquire(context.Background(), *purpose, *wait)
		if err != nil {
			fmt.Fprintln(os.Stderr, "herd slot:", err)
			os.Exit(1)
		}
		fmt.Printf("slot=%d token=%s\n", lease.Slot(), lease.Token())
	case "release":
		fs := flag.NewFlagSet("slot release", flag.ContinueOnError)
		n := fs.Int("slot", -1, "slot number")
		token := fs.String("token", "", "lease token")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := s.Release(*n, *token); err != nil {
			fmt.Fprintln(os.Stderr, "herd slot:", err)
			os.Exit(1)
		}
	case "with":
		withSlot(s, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "herd slot: unknown mode %q\n%s\n", args[0], slotUsage)
		os.Exit(2)
	}
}

func withSlot(s *slot.Semaphore, args []string) {
	purpose, wait := "manual", slot.DefaultTimeout
	for len(args) > 0 && args[0] != "--" {
		switch args[0] {
		case "--purpose":
			if len(args) < 2 {
				os.Exit(2)
			}
			purpose, args = args[1], args[2:]
		case "--wait":
			if len(args) < 2 {
				os.Exit(2)
			}
			d, err := time.ParseDuration(args[1])
			if err != nil {
				os.Exit(2)
			}
			wait, args = d, args[2:]
		default:
			break
		}
		if len(args) > 0 && args[0] != "--" && (args[0] == "--purpose" || args[0] == "--wait") {
			continue
		}
		break
	}
	if len(args) == 0 || args[0] != "--" || len(args) == 1 {
		fmt.Fprintln(os.Stderr, slotUsage)
		os.Exit(2)
	}
	child := args[1:]
	err := s.With(context.Background(), purpose, wait, func() error {
		cmd := exec.Command(child[0], child[1:]...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return cmd.Run()
	})
	if err == nil {
		return
	}
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	if strings.Contains(err.Error(), "slots:") {
		fmt.Fprintln(os.Stderr, "herd slot:", err)
	}
	os.Exit(1)
}
