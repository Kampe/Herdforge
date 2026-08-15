package main

import (
	"testing"
)

// mockPackage builds a goPackage with the given import path and imports.
func mockPackage(path string, imports ...string) goPackage {
	return goPackage{
		ImportPath: path,
		Name:       "mypkg",
		Imports:    imports,
	}
}

// mockMain builds a goPackage representing a main entry point.
func mockMain(path string, imports ...string) goPackage {
	return goPackage{
		ImportPath: path,
		Name:       "main",
		Imports:    imports,
	}
}

// mockTestPackage builds a goPackage with test files that import the given
// test imports.
func mockTestPackage(path string, prodImports, testImports []string) goPackage {
	return goPackage{
		ImportPath:  path,
		Name:        "mypkg",
		Imports:     prodImports,
		TestImports: testImports,
		TestGoFiles: []string{"foo_test.go"},
	}
}

func TestTransitiveDeps(t *testing.T) {
	// Graph: root -> a -> b -> c, root -> d
	// c and d are leaf nodes.
	byPath := map[string]goPackage{
		modulePath + "/cmd/herd": mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a", modulePath+"/pkg/d"),
		modulePath + "/pkg/a":   mockPackage(modulePath+"/pkg/a", modulePath+"/pkg/b"),
		modulePath + "/pkg/b":   mockPackage(modulePath+"/pkg/b", modulePath+"/pkg/c"),
		modulePath + "/pkg/c":   mockPackage(modulePath+"/pkg/c"),
		modulePath + "/pkg/d":   mockPackage(modulePath+"/pkg/d"),
		modulePath + "/pkg/orphan": mockPackage(modulePath+"/pkg/orphan"),
	}

	reach := transitiveDeps(modulePath+"/cmd/herd", byPath, false)

	for _, p := range []string{"/cmd/herd", "/pkg/a", "/pkg/b", "/pkg/c", "/pkg/d"} {
		if !reach[modulePath+p] {
			t.Errorf("transitiveDeps: %s should be reachable", p)
		}
	}
	if reach[modulePath+"/pkg/orphan"] {
		t.Errorf("transitiveDeps: /pkg/orphan should NOT be reachable")
	}
}

func TestTransitiveDepsNoCycles(t *testing.T) {
	// Graph with a cycle: a -> b -> a. Should terminate.
	byPath := map[string]goPackage{
		modulePath + "/cmd/herd": mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		modulePath + "/pkg/a":   mockPackage(modulePath+"/pkg/a", modulePath+"/pkg/b"),
		modulePath + "/pkg/b":   mockPackage(modulePath+"/pkg/b", modulePath+"/pkg/a"),
	}
	reach := transitiveDeps(modulePath+"/cmd/herd", byPath, false)
	if !reach[modulePath+"/pkg/a"] || !reach[modulePath+"/pkg/b"] {
		t.Fatal("transitiveDeps: cyclic graph should still reach all nodes")
	}
}

func TestBuildInventoryClassification(t *testing.T) {
	pkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/wired", modulePath+"/pkg/prodwithtests"),
		mockMain(modulePath+"/cmd/other", modulePath+"/pkg/wired"),
		mockPackage(modulePath+"/pkg/wired"),
		mockTestPackage(modulePath+"/pkg/prodwithtests", nil, []string{modulePath + "/pkg/testhelper"}),
		mockPackage(modulePath+"/pkg/testhelper"),
		mockPackage(modulePath+"/pkg/unwired"),
		mockTestPackage(modulePath+"/internal/testhelper", nil, nil),
	}
	// Give the unwired package its own test files.
	pkgs[5].TestGoFiles = []string{"x_test.go"}

	inv := buildInventory(pkgs)
	byPath := map[string]PackageInfo{}
	for _, p := range inv.Packages {
		byPath[p.Path] = p
	}

	cases := []struct {
		path  string
		class string
	}{
		{modulePath + "/cmd/herd", classProduction},
		{modulePath + "/cmd/other", classSecondary},
		{modulePath + "/pkg/wired", classProduction},
		{modulePath + "/pkg/prodwithtests", classProduction},
		{modulePath + "/pkg/testhelper", classTestHelper},
		{modulePath + "/pkg/unwired", classUnwired},
		{modulePath + "/internal/testhelper", classInternal},
	}
	for _, c := range cases {
		got, ok := byPath[c.path]
		if !ok {
			t.Errorf("buildInventory: %s missing from inventory", c.path)
			continue
		}
		if got.Class != c.class {
			t.Errorf("buildInventory: %s classified as %s, want %s", c.path, got.Class, c.class)
		}
	}
}

func TestBuildInventoryCounts(t *testing.T) {
	pkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockMain(modulePath+"/cmd/other"),
		mockPackage(modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/b"),
		mockTestPackage(modulePath+"/internal/t", nil, nil),
	}
	inv := buildInventory(pkgs)

	if inv.ClassCounts[classProduction] != 2 { // cmd/herd + pkg/a
		t.Errorf("production count = %d, want 2", inv.ClassCounts[classProduction])
	}
	if inv.ClassCounts[classSecondary] != 1 {
		t.Errorf("secondary count = %d, want 1", inv.ClassCounts[classSecondary])
	}
	if inv.ClassCounts[classUnwired] != 1 { // pkg/b
		t.Errorf("unwired count = %d, want 1", inv.ClassCounts[classUnwired])
	}
	if inv.ClassCounts[classInternal] != 1 {
		t.Errorf("internal count = %d, want 1", inv.ClassCounts[classInternal])
	}
}

func TestCheckDriftNoChange(t *testing.T) {
	pkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/b"),
	}
	inv := buildInventory(pkgs)
	if checkDrift(inv, inv) {
		t.Fatal("checkDrift: identical inventories should not drift")
	}
}

func TestCheckDriftProductionRegression(t *testing.T) {
	baselinePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
	}
	baseline := buildInventory(baselinePkgs)

	// Live: pkg/a is no longer imported by cmd/herd (wiring broke).
	livePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd"),
		mockPackage(modulePath+"/pkg/a"),
	}
	live := buildInventory(livePkgs)

	if !checkDrift(baseline, live) {
		t.Fatal("checkDrift: production regression should fail the check")
	}
}

func TestCheckDriftNewUnwiredPackage(t *testing.T) {
	baselinePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
	}
	baseline := buildInventory(baselinePkgs)

	// Live: a new unwired package appeared that was not in the baseline.
	livePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/newunwired"),
	}
	live := buildInventory(livePkgs)

	if !checkDrift(baseline, live) {
		t.Fatal("checkDrift: new unwired package should fail the check")
	}
}

func TestCheckDriftNewProductionPackageAllowed(t *testing.T) {
	baselinePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
	}
	baseline := buildInventory(baselinePkgs)

	// Live: a new production-reachable package is fine (growth, not drift).
	livePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a", modulePath+"/pkg/newprod"),
		mockPackage(modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/newprod"),
	}
	live := buildInventory(livePkgs)

	if checkDrift(baseline, live) {
		t.Fatal("checkDrift: new production package should NOT fail the check")
	}
}

func TestCheckDriftNewSecondaryBinaryAllowed(t *testing.T) {
	baselinePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
	}
	baseline := buildInventory(baselinePkgs)

	// Live: a new secondary binary is fine (not unwired drift).
	livePkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
		mockMain(modulePath+"/cmd/newtool"),
	}
	live := buildInventory(livePkgs)

	if checkDrift(baseline, live) {
		t.Fatal("checkDrift: new secondary binary should NOT fail the check")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	pkgs := []goPackage{
		mockMain(modulePath+"/cmd/herd", modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/a"),
		mockPackage(modulePath+"/pkg/b"),
	}
	inv := buildInventory(pkgs)

	tmp := t.TempDir() + "/baseline.json"
	if err := writeJSON(tmp, inv); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	loaded, err := readJSON(tmp)
	if err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if len(loaded.Packages) != len(inv.Packages) {
		t.Errorf("round-trip: package count %d != %d", len(loaded.Packages), len(inv.Packages))
	}
	if loaded.ClassCounts[classProduction] != inv.ClassCounts[classProduction] {
		t.Errorf("round-trip: production count mismatch")
	}
}
