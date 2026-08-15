// Command package-inventory builds a graph-backed reachability and classification
// inventory of every Go package in the Herdforge module (FAC-301).
//
// It uses `go list` to enumerate the import graph, computes transitive
// reachability from each binary entry point, classifies packages, and either
// generates a checked-in baseline (--generate) or validates the live graph
// against that baseline (--check). The default (no flag) prints a human-readable
// summary to stdout.
//
// Classifications:
//
//   - production      reachable from the primary binary (cmd/herd) non-test build
//   - secondary       a main package that is not the primary binary
//   - test-helper     not production-reachable but imported by tests of production packages
//   - unwired         not production-reachable, not a secondary binary, not a cross-package
//                     test import; has its own tests but is not wired into any binary
//   - internal        under internal/ — preserved test infrastructure, excluded from drift
//
// The check fails (exit 1) when:
//   1. A previously production-reachable package regresses to unwired (broken wiring).
//   2. A new unwired package appears that is not recorded in the baseline (intent tracking).
//
// Usage:
//
//	go run ./scripts/packageinventory/                 # print summary
//	go run ./scripts/packageinventory/ --generate FILE  # write baseline JSON
//	go run ./scripts/packageinventory/ --check FILE     # validate against baseline
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const modulePath = "github.com/Kampe/Herdforge"

// entryPoint is the primary binary whose transitive import closure defines the
// production-reachable set. Secondary binaries (other main packages) are detected
// automatically and tracked separately.
const primaryEntry = modulePath + "/cmd/herd"

// classification labels.
const (
	classProduction  = "production"
	classSecondary   = "secondary"
	classTestHelper  = "test-helper"
	classUnwired     = "unwired"
	classInternal    = "internal"
)

// PackageInfo is the per-package inventory record.
type PackageInfo struct {
	Path          string `json:"path"`
	Class         string `json:"class"`
	TestImporters int    `json:"test_importers"`
	HasOwnTests   bool   `json:"has_own_tests"`
}

// Inventory is the full baseline document.
type Inventory struct {
	Module      string        `json:"module"`
	Primary     string        `json:"primary_entry"`
	Generated   string        `json:"generated_by"`
	ClassCounts map[string]int `json:"class_counts"`
	Packages    []PackageInfo  `json:"packages"`
}

// goPackage mirrors the subset of `go list -json` output we need.
type goPackage struct {
	ImportPath   string   `json:"ImportPath"`
	Name         string   `json:"Name"`
	Imports      []string `json:"Imports"`
	TestImports  []string `json:"TestImports"`
	XTestImports []string `json:"XTestImports"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
	Dir          string   `json:"Dir"`
}

func main() {
	genFile := flag.String("generate", "", "write baseline JSON to FILE")
	checkFile := flag.String("check", "", "validate live graph against baseline FILE")
	flag.Parse()

	if *genFile != "" && *checkFile != "" {
		fmt.Fprintln(os.Stderr, "package-inventory: --generate and --check are mutually exclusive")
		os.Exit(2)
	}

	pkgs, err := listAllPackages()
	if err != nil {
		fmt.Fprintf(os.Stderr, "package-inventory: %v\n", err)
		os.Exit(1)
	}

	inv := buildInventory(pkgs)

	if *genFile != "" {
		if err := writeJSON(*genFile, inv); err != nil {
			fmt.Fprintf(os.Stderr, "package-inventory: write baseline: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "==> Wrote package inventory baseline to %s (%d packages)\n", *genFile, len(inv.Packages))
		return
	}

	if *checkFile != "" {
		baseline, err := readJSON(*checkFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "package-inventory: read baseline: %v\n", err)
			os.Exit(1)
		}
		if failed := checkDrift(baseline, inv); failed {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "==> Package inventory check PASSED (%d packages, %d production, %d unwired)\n",
			len(inv.Packages), inv.ClassCounts[classProduction], inv.ClassCounts[classUnwired])
		return
	}

	printSummary(inv)
}

// listAllPackages runs `go list -json` over the entire module and returns the
// parsed package set. It filters to the current module's import paths.
func listAllPackages() ([]goPackage, error) {
	cmd := exec.Command("go", "list", "-json", "./cmd/...", "./pkg/...", "./internal/...")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []goPackage
	for dec.More() {
		var p goPackage
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if !strings.HasPrefix(p.ImportPath, modulePath+"/") && p.ImportPath != modulePath {
			continue
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// buildInventory computes reachability sets and classifies every package.
func buildInventory(pkgs []goPackage) Inventory {
	byPath := make(map[string]goPackage, len(pkgs))
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}

	// Collect all main packages (entry points).
	var mains []string
	for _, p := range pkgs {
		if p.Name == "main" {
			mains = append(mains, p.ImportPath)
		}
	}
	sort.Strings(mains)

	// Production-reachable: transitive non-test imports of the primary entry point.
	prodReach := transitiveDeps(primaryEntry, byPath, false)

	// Secondary binaries: main packages other than the primary entry.
	secondarySet := map[string]bool{}
	for _, m := range mains {
		if m != primaryEntry {
			secondarySet[m] = true
		}
	}

	// Test-importers count: how many PRODUCTION-reachable packages' test files
	// import each path. A package imported only by tests of other unwired
	// packages is part of the unwired subgraph, not a production test-helper.
	testImporters := map[string]int{}
	for _, p := range pkgs {
		if !prodReach[p.ImportPath] {
			continue
		}
		seen := map[string]bool{}
		testDeps := append(p.TestImports, p.XTestImports...)
		for _, imp := range testDeps {
			if strings.HasPrefix(imp, modulePath+"/") && imp != p.ImportPath && !seen[imp] {
				testImporters[imp]++
				seen[imp] = true
			}
		}
	}

	var infos []PackageInfo
	for _, p := range pkgs {
		path := p.ImportPath
		hasTests := len(p.TestGoFiles) > 0 || len(p.XTestGoFiles) > 0

		var class string
		switch {
		case prodReach[path]:
			class = classProduction
		case secondarySet[path]:
			class = classSecondary
		case strings.HasPrefix(path, modulePath+"/internal/"):
			class = classInternal
		case testImporters[path] > 0:
			class = classTestHelper
		default:
			class = classUnwired
		}

		infos = append(infos, PackageInfo{
			Path:          path,
			Class:         class,
			TestImporters: testImporters[path],
			HasOwnTests:   hasTests,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })

	counts := map[string]int{}
	for _, p := range infos {
		counts[p.Class]++
	}

	return Inventory{
		Module:      modulePath,
		Primary:     primaryEntry,
		Generated:   "scripts/packageinventory/inventory.go",
		ClassCounts: counts,
		Packages:    infos,
	}
}

// transitiveDeps performs a breadth-first traversal of non-test (or test)
// imports starting from root, returning the set of reachable paths (including
// root itself).
func transitiveDeps(root string, byPath map[string]goPackage, includeTests bool) map[string]bool {
	visited := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		p, ok := byPath[cur]
		if !ok {
			continue
		}
		imports := p.Imports
		if includeTests {
			imports = append(append(imports, p.TestImports...), p.XTestImports...)
		}
		for _, imp := range imports {
			if strings.HasPrefix(imp, modulePath+"/") && !visited[imp] {
				queue = append(queue, imp)
			}
		}
	}
	return visited
}

// checkDrift compares the live inventory against the baseline and returns true
// if the check should fail. Two drift conditions are enforced:
//
//  1. Regression: a package that was "production" in the baseline is no longer
//     production-reachable (wiring broke).
//  2. Unintended growth: a new "unwired" package appears that was not in the
//     baseline. Intentionally adding an unwired package requires updating the
//     baseline with --generate.
func checkDrift(baseline, live Inventory) bool {
	baselineByPath := map[string]PackageInfo{}
	for _, p := range baseline.Packages {
		baselineByPath[p.Path] = p
	}
	liveByPath := map[string]PackageInfo{}
	for _, p := range live.Packages {
		liveByPath[p.Path] = p
	}

	failed := false

	// 1. Regression: production package lost its wiring.
	for path, b := range baselineByPath {
		if b.Class == classProduction {
			if l, ok := liveByPath[path]; ok && l.Class != classProduction {
				fmt.Fprintf(os.Stderr, "FAIL package-inventory: %s regressed from %s to %s (broken wiring)\n", path, b.Class, l.Class)
				failed = true
			}
		}
	}

	// 2. Unintended growth: new unwired package not in baseline.
	for _, l := range live.Packages {
		if l.Class != classUnwired {
			continue
		}
		if _, existed := baselineByPath[l.Path]; !existed {
			fmt.Fprintf(os.Stderr, "FAIL package-inventory: new unwired package %s not in baseline (run --generate to record intent)\n", l.Path)
			failed = true
		}
	}

	// 3. Removed package that was in baseline (informational, not a hard failure
	//    since deletion is a deliberate act, but surface it for awareness).
	for path := range baselineByPath {
		if _, ok := liveByPath[path]; !ok {
			fmt.Fprintf(os.Stderr, "WARN package-inventory: %s in baseline but no longer exists (update baseline with --generate)\n", path)
		}
	}

	return failed
}

func writeJSON(path string, inv Inventory) error {
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func readJSON(path string) (Inventory, error) {
	var inv Inventory
	data, err := os.ReadFile(path)
	if err != nil {
		return inv, err
	}
	if err := json.Unmarshal(data, &inv); err != nil {
		return inv, err
	}
	return inv, nil
}

func printSummary(inv Inventory) {
	fmt.Printf("Package Inventory: %s\n", inv.Module)
	fmt.Printf("Primary entry: %s\n\n", inv.Primary)
	fmt.Printf("%-16s %d\n", "production", inv.ClassCounts[classProduction])
	fmt.Printf("%-16s %d\n", "secondary", inv.ClassCounts[classSecondary])
	fmt.Printf("%-16s %d\n", "test-helper", inv.ClassCounts[classTestHelper])
	fmt.Printf("%-16s %d\n", "internal", inv.ClassCounts[classInternal])
	fmt.Printf("%-16s %d\n", "unwired", inv.ClassCounts[classUnwired])
	fmt.Printf("%-16s %d\n", "TOTAL", len(inv.Packages))

	if inv.ClassCounts[classUnwired] > 0 {
		fmt.Println("\nUnwired packages (own tests, not wired into any binary):")
		for _, p := range inv.Packages {
			if p.Class == classUnwired {
				short := strings.TrimPrefix(p.Path, modulePath+"/")
				fmt.Printf("  %-40s tests=%v\n", short, p.HasOwnTests)
			}
		}
	}
}
