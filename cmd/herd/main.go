package main

import (
	"fmt"
	"os"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("herd version %s\n", version)
		os.Exit(0)
	}

	fmt.Println("Herdforge: Standalone Multi-Agent Orchestration Daemon")
	fmt.Println("Run 'herd --help' or see RFC-001 in docs/rfcs/RFC-001-HERD-DAEMON.md")
}
