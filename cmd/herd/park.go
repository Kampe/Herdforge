package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/park"
)

// runPark ports bin/herd-park. `audit` exits non-zero when parked work is
// reachable from nothing but a branch, so a heartbeat surfaces the exposure
// without a human remembering to look.
func runPark() {
	repo := park.Repo{Dir: "."}
	args := os.Args[2:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "audit":
		quiet := len(args) > 1 && args[1] == "--quiet"
		findings, err := repo.Audit()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd park: %v\n", err)
			os.Exit(2)
		}
		exposed := park.ExposedCount(findings)
		if !quiet {
			if len(findings) == 0 {
				fmt.Println("herd park: no parked work found")
			}
			for _, f := range findings {
				tags := strings.Join(f.Tags, ",")
				if tags == "" {
					tags = "(branch only)"
				}
				fmt.Printf("%-14s %s  %-28s %-24s %s\n", f.Durability, f.SHA, f.Branch, tags, f.Subject)
			}
			fmt.Printf("herd park: %d parked, %d NOT durable\n", len(findings), exposed)
		}
		if exposed > 0 {
			os.Exit(1)
		}

	case "list":
		rows, err := repo.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd park: %v\n", err)
			os.Exit(2)
		}
		if len(rows) == 0 {
			fmt.Println("herd park: no parked tags")
			return
		}
		for _, r := range rows {
			fmt.Printf("  %s\n", r)
		}

	case "", "-h", "--help":
		fmt.Println("usage: herd park <slug> <sha> -m <message>   park one commit durably")
		fmt.Println("       herd park audit [--quiet]              report parked work that is NOT durable")
		fmt.Println("       herd park list                         show every parked tag and its subject")
		fmt.Println("  A parked commit reachable only from a branch is one reset --hard from gone.")

	default:
		// park <slug> <sha> -m <message>
		if len(args) < 4 || args[2] != "-m" {
			fmt.Fprintln(os.Stderr, "usage: herd park <slug> <sha> -m <message>")
			os.Exit(2)
		}
		tag, err := repo.Park(args[0], args[1], strings.Join(args[3:], " "))
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd park: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd park: %s is durable (annotated tag %s, pushed to origin)\n", args[1], tag)
	}
}
