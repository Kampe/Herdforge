// Command check-coverage-floors enforces non-vacuous package coverage floors
// for FAC-135. Exit 0 only when every listed package meets its floor.
//
// Usage: go run ./scripts/check-coverage-floors.go <coverprofile>
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// floors maps import-path prefix → minimum average function coverage percent.
// These are deliberately modest but non-zero. Never lower a floor to silence
// a regression; raise them as suites grow.
var floors = map[string]float64{
	"github.com/Kampe/Herdforge/pkg/lifecycle": 40,
	"github.com/Kampe/Herdforge/pkg/daemon":    40,
	"github.com/Kampe/Herdforge/pkg/preflight": 50,
	"github.com/Kampe/Herdforge/pkg/provider":  30,
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: check-coverage-floors <coverprofile>")
		os.Exit(2)
	}
	profile := os.Args[1]
	f, err := os.Open(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open coverprofile: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// go cover profile format: mode: set|count|atomic then
	// file:startLine.startCol,endLine.endCol numStmt count
	type acc struct{ stmts, covered int }
	byPkg := map[string]*acc{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// path.go:N.N,N.N NS C
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		fileCount := fields[0]
		colon := strings.Index(fileCount, ":")
		if colon < 0 {
			continue
		}
		file := fileCount[:colon]
		numStmt, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || numStmt <= 0 {
			continue
		}
		pkg := filepath.ToSlash(filepath.Dir(file))
		a := byPkg[pkg]
		if a == nil {
			a = &acc{}
			byPkg[pkg] = a
		}
		a.stmts += numStmt
		if count > 0 {
			a.covered += numStmt
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read coverprofile: %v\n", err)
		os.Exit(1)
	}

	failed := false
	for pkg, floor := range floors {
		a := byPkg[pkg]
		if a == nil || a.stmts == 0 {
			fmt.Fprintf(os.Stderr, "FAIL coverage floor: %s has no statements in profile (floor %.0f%%)\n", pkg, floor)
			failed = true
			continue
		}
		pct := 100 * float64(a.covered) / float64(a.stmts)
		if pct+1e-9 < floor {
			fmt.Fprintf(os.Stderr, "FAIL coverage floor: %s at %.1f%% < %.0f%%\n", pkg, pct, floor)
			failed = true
			continue
		}
		fmt.Printf("OK   %s %.1f%% (floor %.0f%%)\n", pkg, pct, floor)
	}
	if failed {
		os.Exit(1)
	}
}
