package verifier

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The source manifest budget.
//
// The regression these cover: the total budget was 16 MiB and this
// repository's own tracked tree reached 16,749,519 bytes, leaving 27,697
// bytes of headroom. CI validates a pull request's MERGE ref rather than its
// head, so a merge tree of 16,809,993 bytes was refused while both of its
// parents passed on their own, and the next change of any size would have
// done the same to whoever authored it.
//
// Everything here drives the REAL admission path with an injected tiny
// budget. That is deliberate: reaching a ceiling by allocating it would make
// these tests cost megabytes to prove arithmetic, and a test that is
// expensive to run is a test that stops being run. Nothing below needs
// Docker, and nothing below asserts anything about the host's current tree,
// so the regression stays reproducible long after this tree's size changes.

// budgetArchiveMember renders one well-formed regular-file member. Metadata is
// what git archive emits, so these fixtures reach the budget check rather than
// dying earlier on ownership or mode validation.
func budgetArchiveMember(t *testing.T, name string, size int) []byte {
	t.Helper()
	var member bytes.Buffer
	writer := tar.NewWriter(&member)
	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(size), Uid: 0, Gid: 0}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if _, err := writer.Write(bytes.Repeat([]byte{'x'}, size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Drop the writer's trailing end-of-archive block so members concatenate.
	return member.Bytes()[:member.Len()-1024]
}

func budgetArchive(t *testing.T, members ...[]byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	for _, member := range members {
		archive.Write(member)
	}
	archive.Write(make([]byte, 1024))
	return archive.Bytes()
}

func tinyBudget(fileBytes, totalBytes int64) sourceManifestBudget {
	return sourceManifestBudget{fileBytes: fileBytes, totalBytes: totalBytes}
}

// TestManifestBudgetAdmitsExactlyAtTheTotalAndRefusesOneByteOver is the
// boundary itself. A tree that exactly fills the budget is legitimate source
// and must build; one byte more must refuse.
func TestManifestBudgetAdmitsExactlyAtTheTotalAndRefusesOneByteOver(t *testing.T) {
	budget := tinyBudget(64, 100)

	atLimit := budgetArchive(t, budgetArchiveMember(t, "a.go", 60), budgetArchiveMember(t, "b.go", 40))
	if _, err := sourceManifestDigestFromArchiveWithBudget(atLimit, budget); err != nil {
		t.Fatalf("a manifest of exactly the budget is legitimate source and must be admitted: %v", err)
	}

	overByOne := budgetArchive(t, budgetArchiveMember(t, "a.go", 60), budgetArchiveMember(t, "b.go", 41))
	_, err := sourceManifestDigestFromArchiveWithBudget(overByOne, budget)
	if err == nil {
		t.Fatal("one byte over the total budget must refuse")
	}
	for _, want := range []string{"candidate archive", "b.go", "101", "100", "total source manifest budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q so an operator can act on it, got: %v", want, err)
		}
	}
}

// TestManifestBudgetRefusesAggregateOfManySmallFiles is the shape of the real
// failure: no single member is anywhere near the per-file limit, and the tree
// still crosses the total. A budget that only ever looked at one file at a
// time would have admitted this.
func TestManifestBudgetRefusesAggregateOfManySmallFiles(t *testing.T) {
	budget := tinyBudget(1024, 100)
	members := make([][]byte, 0, 11)
	for i := 0; i < 11; i++ {
		members = append(members, budgetArchiveMember(t, "pkg/small"+string(rune('a'+i))+".go", 10))
	}
	_, err := sourceManifestDigestFromArchiveWithBudget(budgetArchive(t, members...), budget)
	if err == nil {
		t.Fatal("many small files crossing the total must refuse even when every file is far under the per-file limit")
	}
	if strings.Contains(err.Error(), "per-file") {
		t.Errorf("aggregate overflow must be reported against the TOTAL budget, not the per-file one: %v", err)
	}
	if !strings.Contains(err.Error(), "total source manifest budget") {
		t.Errorf("refusal must name the total budget: %v", err)
	}
}

// TestManifestBudgetRefusesSingleFileOverThePerFileLimit keeps the inner
// ceiling meaningful: one oversized member is refused on its own terms, with
// the per-file category named, even when the total still has room.
func TestManifestBudgetRefusesSingleFileOverThePerFileLimit(t *testing.T) {
	budget := tinyBudget(32, 1024)
	_, err := sourceManifestDigestFromArchiveWithBudget(
		budgetArchive(t, budgetArchiveMember(t, "big.bin", 33)), budget)
	if err == nil {
		t.Fatal("a member over the per-file budget must refuse even with total headroom to spare")
	}
	for _, want := range []string{"per-file source manifest budget", "big.bin", "33", "32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q, got: %v", want, err)
		}
	}
}

// TestManifestBudgetRefusalNamesNoHostPath: these strings land in CI output.
// The manifest path is archive-relative by construction and must stay that way.
func TestManifestBudgetRefusalNamesNoHostPath(t *testing.T) {
	budget := tinyBudget(8, 8)
	_, err := sourceManifestDigestFromArchiveWithBudget(
		budgetArchive(t, budgetArchiveMember(t, "pkg/verifier/x.go", 9)), budget)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.HasPrefix(err.Error(), "/") || strings.Contains(err.Error(), " /") {
		t.Errorf("refusal leaked an absolute host path: %v", err)
	}
	if !strings.Contains(err.Error(), "pkg/verifier/x.go") {
		t.Errorf("refusal must still name the archive-relative member: %v", err)
	}
}

// TestManifestBudgetAdmitChecksOverflowBeforeAdding: the total check must not
// be expressible as total+size, or a corrupt or hostile size can wrap the
// accumulator and read as comfortably under budget.
func TestManifestBudgetAdmitChecksOverflowBeforeAdding(t *testing.T) {
	budget := tinyBudget(1<<62, 1<<62)
	const nearMax = int64(1)<<62 - 1
	if err := budget.admit("candidate archive", "file", "huge.bin", nearMax, nearMax); err == nil {
		t.Fatal("a total that would overflow int64 must refuse, not wrap into looking small")
	}
	if err := budget.admit("candidate archive", "file", "negative.bin", -1, 0); err == nil {
		t.Fatal("a negative declared size must refuse")
	}
	if err := budget.admit("candidate archive", "file", "exact.bin", 10, 0); err != nil {
		t.Fatalf("a member inside both budgets must be admitted: %v", err)
	}
}

// TestManifestBudgetProducerAndReadbackAgree is the parity contract: the
// archive producer and the filesystem readback share one policy, so a tree
// either passes both or fails both. Drift between them would let a source set
// be archived and then refused on the way back, or worse, the reverse.
func TestManifestBudgetProducerAndReadbackAgree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int{"pkg/a.go": 60, "pkg/b.go": 40}
	for name, size := range sizes {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), bytes.Repeat([]byte{'x'}, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	members, err := collectFilesystemReadbackMembers(root)
	if err != nil {
		t.Fatal(err)
	}
	archive := budgetArchive(t,
		budgetArchiveMember(t, "pkg/a.go", sizes["pkg/a.go"]),
		budgetArchiveMember(t, "pkg/b.go", sizes["pkg/b.go"]),
	)

	for _, tc := range []struct {
		name   string
		budget sourceManifestBudget
		admit  bool
	}{
		{"exactly at the total", tinyBudget(64, 100), true},
		{"one byte under the total", tinyBudget(64, 99), false},
		{"per-file too small for either side", tinyBudget(50, 1024), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, archiveErr := sourceManifestDigestFromArchiveWithBudget(archive, tc.budget)
			_, readbackErr := sourceManifestDigestFromFilesystemMembersWithBudget(root, members, tc.budget)
			if (archiveErr == nil) != (readbackErr == nil) {
				t.Fatalf("producer and readback disagreed under one budget: archive=%v readback=%v", archiveErr, readbackErr)
			}
			if tc.admit && archiveErr != nil {
				t.Fatalf("both sides should admit: %v", archiveErr)
			}
			if !tc.admit && archiveErr == nil {
				t.Fatal("both sides should refuse")
			}
		})
	}
}

// TestDefaultManifestBudgetLeavesRealHeadroomForAMergedTree is the guard on
// the constant itself, written so it never asserts on the host's tree: it
// states the sizes the incident actually produced and requires the shipped
// budget to clear the largest of them with genuine room to grow.
func TestDefaultManifestBudgetLeavesRealHeadroomForAMergedTree(t *testing.T) {
	const (
		observedMergedTreeBytes = int64(16_809_993) // refs/pull/830/merge
		observedMainTreeBytes   = int64(16_749_519) // main at the time
	)
	budget := defaultSourceManifestBudget()
	if budget.totalBytes <= observedMergedTreeBytes {
		t.Fatalf("total budget %d does not admit the merged tree that failed CI (%d)", budget.totalBytes, observedMergedTreeBytes)
	}
	// A budget that merely clears the incident would fail again on the next
	// merge. Require room for the tree to roughly double.
	if budget.totalBytes < 2*observedMergedTreeBytes {
		t.Fatalf("total budget %d leaves less than double the observed merged tree (%d); the next merges would hit it again",
			budget.totalBytes, observedMergedTreeBytes)
	}
	if budget.totalBytes <= observedMainTreeBytes {
		t.Fatalf("total budget %d does not admit the observed main tree (%d)", budget.totalBytes, observedMainTreeBytes)
	}
	// The per-file ceiling must stay a real inner bound, not be raised to the
	// total by accident.
	if budget.fileBytes <= 0 || budget.fileBytes >= budget.totalBytes {
		t.Fatalf("per-file budget %d must stay a meaningful bound under the total %d", budget.fileBytes, budget.totalBytes)
	}
	// Every bound stays finite; a budget is a ceiling, not a switch.
	if budget.totalBytes <= 0 || maxHermeticSourceTransportBytes <= 0 ||
		maxHermeticSourceTransportMembers <= 0 || maxHermeticSourceTransportPathBytes <= 0 {
		t.Fatal("every transport and manifest bound must remain finite and positive")
	}
	if maxHermeticSourceTransportBytes <= budget.totalBytes {
		t.Fatalf("transport ceiling %d must exceed the manifest total %d it carries",
			maxHermeticSourceTransportBytes, budget.totalBytes)
	}
}

// TestManifestBudgetSymlinkTargetsShareTheSameBudget keeps symlink accounting
// on the one policy: a target's bytes count toward the same total, and its
// refusal is as specific as a file's.
func TestManifestBudgetSymlinkTargetsShareTheSameBudget(t *testing.T) {
	budget := tinyBudget(4, 4)
	if err := budget.admit("candidate archive", "symlink target", "nested/link", 5, 0); err == nil {
		t.Fatal("an oversized symlink target must refuse")
	} else if !strings.Contains(err.Error(), "symlink target") || !strings.Contains(err.Error(), "nested/link") {
		t.Errorf("symlink refusal must name its category and member: %v", err)
	}
	if err := budget.admit("copied source", "symlink target", "nested/link", 3, 2); err == nil {
		t.Fatal("a symlink target crossing the running total must refuse")
	} else if !strings.Contains(err.Error(), "copied source") {
		t.Errorf("readback refusal must name its own surface: %v", err)
	}
}
