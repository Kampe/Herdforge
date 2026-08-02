package overlap

import (
	"context"
	"encoding/json"
	"testing"
)

func TestExtractDecls_AddedFunctionSymbols(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore more\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "work")
	commitFile(t, dir, "app.ts", "export function hotfix() {}\n", "add fn")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	decls, err := o.ExtractDecls(context.Background(), "work", "origin/main")
	if err != nil {
		t.Fatalf("ExtractDecls: %v", err)
	}
	if len(decls) != 1 {
		t.Fatalf("expected 1 decl, got %v", decls)
	}
	if decls[0].Symbol != "hotfix" {
		t.Fatalf("expected symbol hotfix, got %q", decls[0].Symbol)
	}
	if decls[0].Branch != "work" {
		t.Fatalf("expected branch work, got %q", decls[0].Branch)
	}
	if decls[0].Location != "app.ts:1" {
		t.Fatalf("expected app.ts:1, got %q", decls[0].Location)
	}
}

func TestExtractSymbols_SameFileNotHot(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "alpha")
	commitFile(t, dir, "app.ts", "export function hotfix() {}\n", "alpha fn")
	checkoutBranch(t, dir, "main")
	createBranch(t, dir, "beta")
	commitFile(t, dir, "app.ts", "export function hotfix() {}\n", "beta fn too")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	hots := o.SymbolOverlaps(context.Background(), "origin/main", 2)
	if len(hots) != 0 {
		t.Fatalf("same file on two tips must not be hot, got %v", hots)
	}
}

func TestExtractSymbols_TwoFilesDifferentTipsHot(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "alpha")
	commitFile(t, dir, "a.ts", "export function hotfix() {}\n", "alpha")
	checkoutBranch(t, dir, "main")
	createBranch(t, dir, "beta")
	commitFile(t, dir, "b.ts", "export function hotfix() {}\n", "beta")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	hots := o.SymbolOverlaps(context.Background(), "origin/main", 2)
	if len(hots) != 1 {
		t.Fatalf("expected 1 hot symbol, got %v", hots)
	}
	h := hots[0]
	if h.Symbol != "hotfix" {
		t.Fatalf("expected hotfix, got %q", h.Symbol)
	}
	if h.Tips != 2 {
		t.Fatalf("expected 2 tips, got %d", h.Tips)
	}
	if h.Files != 2 {
		t.Fatalf("expected 2 files, got %d", h.Files)
	}
	if len(h.Refs) != 2 {
		t.Fatalf("expected 2 refs, got %v", h.Refs)
	}
}

func TestSymbols_ControlKeywordsExcluded(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	// `if (x)` is a control statement, never a symbol.
	createBranch(t, dir, "work")
	commitFile(t, dir, "ctl.ts", "export function keep() {}\nif (x) { return 1; }\n", "ctl")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	decls, err := o.ExtractDecls(context.Background(), "work", "origin/main")
	if err != nil {
		t.Fatalf("ExtractDecls: %v", err)
	}
	for _, d := range decls {
		if d.Symbol == "if" {
			t.Fatalf("control keyword if leaked as a decl: %v", decls)
		}
	}
	found := false
	for _, d := range decls {
		if d.Symbol == "keep" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected keep to survive, got %v", decls)
	}
}

func TestExtractDecls_LineNumbers(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	// Five added lines: two decls on the 2nd and 5th added lines.
	createBranch(t, dir, "work")
	commitFile(t, dir, "num.ts", "a\nfunction ju(){}\nb\nc\nexport function bee() {}\n", "nl")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	decls, err := o.ExtractDecls(context.Background(), "work", "origin/main")
	if err != nil {
		t.Fatalf("ExtractDecls: %v", err)
	}
	byName := map[string]string{}
	for _, d := range decls {
		byName[d.Symbol] = d.Location
	}
	if byName["ju"] != "num.ts:2" {
		t.Fatalf("expected num.ts:2 for ju, got %v", byName)
	}
	if byName["bee"] != "num.ts:5" {
		t.Fatalf("expected num.ts:5 for bee, got %v", byName)
	}
}

func TestExtractSymbols_Deterministic(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "alpha")
	commitFile(t, dir, "a.ts", "export function hotfix() {}\n", "alpha")
	checkoutBranch(t, dir, "main")
	createBranch(t, dir, "beta")
	commitFile(t, dir, "b.ts", "export function hotfix() {}\n", "beta")
	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	hots1 := o.SymbolOverlaps(context.Background(), "origin/main", 2)
	hots2 := o.SymbolOverlaps(context.Background(), "origin/main", 2)
	j1, _ := json.Marshal(hots1)
	j2, _ := json.Marshal(hots2)
	if string(j1) != string(j2) {
		t.Fatalf("non-deterministic symbol overlaps:\n%s\n%s", j1, j2)
	}
	if len(hots1) != 1 {
		t.Fatalf("expected 1 hot, got %v", hots1)
	}
}