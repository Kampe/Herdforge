package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

const nativeReviewTargetTimeout = "90s"

var nativeReviewTestFuncRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

func isNativeReviewHeavyPackage(pkg string) bool {
	parts := strings.Split(strings.Trim(strings.TrimSpace(pkg), "./"), "/")
	if len(parts) < 2 {
		return false
	}
	return (parts[0] == "cmd" && parts[1] == "herd") || (parts[0] == "pkg" && parts[1] == "herdr")
}

func nativeReviewHostedGateReuse() string {
	return `Cite hosted CI verification.test_command / make test-unit / required collector "Build, Preflight & Test Suite" for this exact SHA. Do not run go test ./... or a full-package go test of cmd/herd or pkg/herdr without -run as targeted-first.`
}

func nativeReviewTestFuncNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, m := range nativeReviewTestFuncRe.FindAllStringSubmatch(src, -1) {
		if len(m) < 2 || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

func extractNativeReviewTestNames(worktree string, changed []string) []string {
	seen := map[string]bool{}
	var names []string
	root := strings.TrimSpace(worktree)
	for _, rel := range changed {
		rel = strings.TrimSpace(rel)
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		path := rel
		if root != "" && !filepath.IsAbs(rel) {
			path = filepath.Join(root, rel)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, name := range nativeReviewTestFuncNames(string(body)) {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// formatNativeReviewTargetedCommand is targeted-first native-review policy:
// named tests (-run) for heavy packages. Full-package go test of cmd/herd or
// pkg/herdr is the hosted CI contract, not the first native-review check.
func formatNativeReviewTargetedCommand(pkgs, testNames []string) string {
	var heavy, light []string
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isNativeReviewHeavyPackage(p) {
			heavy = append(heavy, p)
		} else {
			light = append(light, p)
		}
	}
	sort.Strings(heavy)
	sort.Strings(light)
	if len(heavy) == 0 && len(light) == 0 {
		return nativeReviewHostedGateReuse()
	}
	var parts []string
	if len(heavy) > 0 {
		if len(testNames) > 0 {
			parts = append(parts, fmt.Sprintf("GOMAXPROCS=2 go test -count=1 -p=1 -timeout=%s -run %s %s",
				nativeReviewTargetTimeout, strings.Join(testNames, "|"), strings.Join(heavy, " ")))
		} else {
			parts = append(parts, "Select Test names from the diff, then GOMAXPROCS=2 go test -count=1 -p=1 -timeout="+nativeReviewTargetTimeout+" -run TestName "+strings.Join(heavy, " ")+". Full-package go test of those trees without -run is hosted CI, not targeted-first.")
		}
	}
	if len(light) > 0 {
		parts = append(parts, "go test -count=1 -timeout="+nativeReviewTargetTimeout+" "+strings.Join(light, " "))
	}
	return strings.Join(parts, " ; ")
}

func nativeReviewCommandIsFullHeavyPackage(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" || strings.Contains(c, "-run") {
		return false
	}
	fields := strings.Fields(c)
	hasGoTest := false
	for _, f := range fields {
		if f == "test" {
			hasGoTest = true
		}
		if hasGoTest && isNativeReviewHeavyPackage(f) {
			return true
		}
	}
	return false
}

// reviewVerificationBudgetSection is the native-review verification policy.
// Mandatory checks stay: inspect the exact diff, run targeted named tests, file a
// verdict. Full unit suites remain the hosted CI contract
// (verification.test_command / make test-unit / required collector job
// "Build, Preflight & Test Suite"). Native review serializes and budgets;
// it does not fan out overlapping suites.
func reviewVerificationBudgetSection(scopedTestCmd string) string {
	scoped := strings.TrimSpace(scopedTestCmd)
	if scoped == "" {
		scoped = nativeReviewHostedGateReuse()
	}
	return fmt.Sprintf(`VERIFICATION BUDGET — serialize, do not fan out
Launch env is GOMAXPROCS=%s and GOFLAGS includes %s (other GOFLAGS are kept). Do not raise GOMAXPROCS or go -p.

Mandatory checks, one process at a time, in this order:
1. Inspect only git diff origin/main..HEAD (or the packet base..HEAD).
2. Targeted tests: %s
   Targeted-first means named tests (-run Test...). A full-package go test of cmd/herd or pkg/herdr without -run is not targeted.
3. Never start make test-unit, a full-package go test of cmd/herd or pkg/herdr without -run, or go test ./... as the first native-review check, and never start them together. They overlap. Concurrent redundant suites drove native review CPU admission to REFUSE.

Hosted CI owns the full unit suite (configured verification.test_command / make test-unit) and required collector job "Build, Preflight & Test Suite". If those passed for this exact SHA, cite the run. That is honest reuse. Do not re-run the full suite locally during native review unless targeted checks are inconclusive, and then only after they finish.

AGENTS.md package-grid commands are builder/CI instructions, not a native-review fan-out list.
`, herdr.ReviewBudgetGOMAXPROCS, herdr.ReviewBudgetGOFLAGS, scoped)
}
