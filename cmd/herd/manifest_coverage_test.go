package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEverySwitchCaseIsAdmitted is the FAC-573 gate.
//
// `case "pool":` existed in main's switch and "pool" was absent from the control
// surface, so admitRoutedCommand rejected it as an unknown subcommand before the
// handler could run. Warm-pool leases could therefore be TAKEN and never
// RELEASED: two were stuck held in pool.json with no supported way to free them.
//
// This is the fourth command this session that existed but was unreachable
// (candidate, handoffs, transcript, pool). Registration is easy to forget and
// invisible until an operator needs the command, so it is checked mechanically
// rather than remembered.
func TestEverySwitchCaseIsAdmitted(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	// Scope to the TOP-LEVEL dispatch switch. A first attempt matched every
	// one-tab `case` in the file and swept up nested subcommand switches
	// (acquire/release under lock, get/verdict under task), which are not
	// top-level commands and must not be admitted.
	text := string(src)
	start := strings.Index(text, "switch command {")
	if start < 0 {
		t.Fatal("cannot locate the dispatch switch")
	}
	// The dispatch switch ends at its own default clause.
	end := strings.Index(text[start:], "\n\tdefault:")
	if end < 0 {
		t.Fatal("cannot locate the end of the dispatch switch")
	}
	block := text[start : start+end]
	cases := regexp.MustCompile(`(?m)^\tcase "([a-z][a-z0-9-]*)"`).FindAllStringSubmatch(block, -1)
	if len(cases) < 20 {
		t.Fatalf("expected the dispatch switch; matched only %d cases", len(cases))
	}
	known := map[string]bool{}
	for _, name := range knownSubcommands() {
		known[name] = true
	}
	var missing []string
	for _, m := range cases {
		name := m[1]
		// Flags and help sentinels are not subcommands.
		if name == "help" || name == "version" {
			continue
		}
		if !known[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("these subcommands have a handler but are NOT admitted, so they are unreachable: %s\n"+
			"add them to commandNamesByClass and bump controlSurfaceVersion with the new fingerprint",
			strings.Join(missing, ", "))
	}
}
