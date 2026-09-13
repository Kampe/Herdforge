package verifier

import (
	"archive/tar"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Source manifest budget. Every case drives the real admission path with a
// small injected budget rather than allocating a ceiling, and asserts the
// budget-specific refusal so an unrelated metadata error cannot stand in for
// a budget refusal.

const budgetErrMarker = "source manifest budget"

func tinyBudget(fileBytes, totalBytes int64) sourceManifestBudget {
	return sourceManifestBudget{fileBytes: fileBytes, totalBytes: totalBytes}
}

// budgetTarMember renders one well-formed member with the metadata git archive
// emits, so a fixture reaches the budget check instead of dying earlier.
func budgetTarMember(t *testing.T, header tar.Header, size int) []byte {
	t.Helper()
	var member bytes.Buffer
	writer := tar.NewWriter(&member)
	header.Size = int64(size)
	if err := writer.WriteHeader(&header); err != nil {
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
	// Drop the end-of-archive block so members concatenate.
	return member.Bytes()[:member.Len()-1024]
}

func budgetTarFile(t *testing.T, name string, size int) []byte {
	t.Helper()
	return budgetTarMember(t, tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, size)
}

func budgetTarSymlink(t *testing.T, name, target string) []byte {
	t.Helper()
	return budgetTarMember(t, tar.Header{Name: name, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: target}, 0)
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

// budgetFilesystemFile writes real content and pairs it with a root-owned
// FileInfo, because the readback's ownership gate refuses anything else and a
// test-owned file would fail there instead of at the budget.
func budgetFilesystemFile(t *testing.T, root, name string, size int) filesystemManifestMember {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, bytes.Repeat([]byte{'x'}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return filesystemManifestMember{name: name, info: newFAC198RegularInfo(int64(size))}
}

func budgetFilesystemSymlink(name, target string) filesystemManifestMember {
	return filesystemManifestMember{name: name, info: newFAC198SymlinkInfo(), target: target}
}

func requireBudgetRefusal(t *testing.T, side string, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal", side)
	}
	if !strings.Contains(err.Error(), budgetErrMarker) {
		t.Fatalf("%s: refusal is not the budget's, so this case proves nothing: %v", side, err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: refusal must name %q, got: %v", side, want, err)
		}
	}
}

// TestManifestBudgetAdmitsExactlyAtTheTotalAndRefusesOneByteOver: a tree that
// exactly fills the budget is legitimate source; one byte more refuses.
func TestManifestBudgetAdmitsExactlyAtTheTotalAndRefusesOneByteOver(t *testing.T) {
	budget := tinyBudget(64, 100)

	atLimit := budgetArchive(t, budgetTarFile(t, "a.go", 60), budgetTarFile(t, "b.go", 40))
	digest, err := sourceManifestDigestFromArchiveWithBudget(atLimit, budget)
	if err != nil {
		t.Fatalf("a manifest of exactly the budget must be admitted: %v", err)
	}
	if digest == "" {
		t.Fatal("an admitted manifest must produce a digest")
	}

	overByOne := budgetArchive(t, budgetTarFile(t, "a.go", 60), budgetTarFile(t, "b.go", 41))
	_, err = sourceManifestDigestFromArchiveWithBudget(overByOne, budget)
	requireBudgetRefusal(t, "archive", err, "candidate archive", "b.go", "41", "100", "total")
}

// TestManifestBudgetRefusesAggregateOfManySmallFiles is the real failure's
// shape: no member is near the per-file limit and the tree still crosses.
func TestManifestBudgetRefusesAggregateOfManySmallFiles(t *testing.T) {
	budget := tinyBudget(1024, 100)
	members := make([][]byte, 0, 11)
	for i := 0; i < 11; i++ {
		members = append(members, budgetTarFile(t, "pkg/small"+string(rune('a'+i))+".go", 10))
	}
	_, err := sourceManifestDigestFromArchiveWithBudget(budgetArchive(t, members...), budget)
	requireBudgetRefusal(t, "archive", err, "total")
	if strings.Contains(err.Error(), "per-file") {
		t.Errorf("aggregate overflow must be reported against the total budget: %v", err)
	}
}

// TestManifestBudgetRefusesSingleFileOverThePerFileLimit keeps the inner
// ceiling meaningful when the total still has room.
func TestManifestBudgetRefusesSingleFileOverThePerFileLimit(t *testing.T) {
	_, err := sourceManifestDigestFromArchiveWithBudget(
		budgetArchive(t, budgetTarFile(t, "big.bin", 33)), tinyBudget(32, 1024))
	requireBudgetRefusal(t, "archive", err, "per-file", "big.bin", "33", "32")
}

// TestManifestBudgetRefusalNamesNoHostPath: these strings land in CI output.
func TestManifestBudgetRefusalNamesNoHostPath(t *testing.T) {
	_, err := sourceManifestDigestFromArchiveWithBudget(
		budgetArchive(t, budgetTarFile(t, "pkg/verifier/x.go", 9)), tinyBudget(8, 8))
	requireBudgetRefusal(t, "archive", err, "pkg/verifier/x.go")
	if strings.HasPrefix(err.Error(), "/") || strings.Contains(err.Error(), " /") {
		t.Errorf("refusal leaked an absolute host path: %v", err)
	}
}

// TestManifestBudgetAdmitRefusesGenuineInt64Overflow pins the invariant that
// the total is never checked or reported as total+size.
//
// The arithmetic matters: the pair must actually wrap. total = MaxInt64-1 and
// size = 2 sum to MaxInt64+1, which wraps to MinInt64 and reads as far under
// any ceiling, so a regression written as total+size > limit ADMITS it. Both
// values stay inside the per-file limit here, so the per-file check cannot
// refuse first and mask the result.
func TestManifestBudgetAdmitRefusesGenuineInt64Overflow(t *testing.T) {
	budget := tinyBudget(math.MaxInt64, math.MaxInt64)
	// Variables, not constants: Go evaluates constant arithmetic exactly, so a
	// constant total+size here would not compile rather than wrap.
	var total int64 = math.MaxInt64 - 1
	var size int64 = 2

	if total+size >= 0 {
		t.Fatalf("fixture does not overflow: total+size = %d, so this case cannot detect the bug", total+size)
	}
	err := budget.admit("candidate archive", "file", "wrap.bin", size, total)
	requireBudgetRefusal(t, "admit", err, "wrap.bin")
	// The message must not have computed the wrapped sum either: MaxInt64-1
	// plus 2 wraps to MinInt64, so any negative total in the text is the bug.
	if strings.Contains(err.Error(), "-92233720368547758") {
		t.Errorf("diagnostic computed the overflowed total: %v", err)
	}

	// Controls: the same budget admits a member that genuinely fits, and a
	// negative declared size is refused outright.
	if err := budget.admit("candidate archive", "file", "fits.bin", 2, 0); err != nil {
		t.Fatalf("a member inside both budgets must be admitted: %v", err)
	}
	if err := budget.admit("candidate archive", "file", "negative.bin", -1, 0); err == nil {
		t.Fatal("a negative declared size must refuse")
	}
}

// budgetParityCase runs one budget through both policy sites over the same
// logical member set and asserts they agree: equal digests when admitted,
// budget refusals on BOTH sides when not.
func budgetParityCase(t *testing.T, archive []byte, root string, members []filesystemManifestMember, budget sourceManifestBudget, admit bool) {
	t.Helper()
	archiveDigest, archiveErr := sourceManifestDigestFromArchiveWithBudget(archive, budget)
	readbackDigest, readbackErr := sourceManifestDigestFromFilesystemMembersWithBudget(root, members, budget)
	if (archiveErr == nil) != (readbackErr == nil) {
		t.Fatalf("producer and readback disagreed: archive=%v readback=%v", archiveErr, readbackErr)
	}
	if admit {
		if archiveErr != nil {
			t.Fatalf("both sides should admit: %v", archiveErr)
		}
		if archiveDigest == "" || archiveDigest != readbackDigest {
			t.Fatalf("admitted manifests must produce one identical digest: archive=%q readback=%q", archiveDigest, readbackDigest)
		}
		return
	}
	requireBudgetRefusal(t, "archive", archiveErr)
	requireBudgetRefusal(t, "readback", readbackErr)
}

// TestManifestBudgetFileParityAcrossProducerAndReadback: one policy, one
// digest, and refusals that are the budget's on both sides.
func TestManifestBudgetFileParityAcrossProducerAndReadback(t *testing.T) {
	if !fac198OwnerFixtureSupported() {
		t.Skip("readback ownership fixture is unavailable on this platform")
	}
	root := t.TempDir()
	members := []filesystemManifestMember{
		budgetFilesystemFile(t, root, "pkg/a.go", 60),
		budgetFilesystemFile(t, root, "pkg/b.go", 40),
	}
	archive := budgetArchive(t, budgetTarFile(t, "pkg/a.go", 60), budgetTarFile(t, "pkg/b.go", 40))

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
			budgetParityCase(t, archive, root, members, tc.budget, tc.admit)
		})
	}
}

// TestManifestBudgetSymlinkParityAcrossProducerAndReadback drives real symlink
// members through BOTH production call sites, so dropping the bound at either
// one is caught. A positive control reaches the same path first.
func TestManifestBudgetSymlinkParityAcrossProducerAndReadback(t *testing.T) {
	if !fac198OwnerFixtureSupported() {
		t.Skip("readback ownership fixture is unavailable on this platform")
	}
	const target = "sibling.go"
	root := t.TempDir()
	members := []filesystemManifestMember{budgetFilesystemSymlink("nested/link", target)}
	archive := budgetArchive(t, budgetTarSymlink(t, "nested/link", target))

	for _, tc := range []struct {
		name   string
		budget sourceManifestBudget
		admit  bool
	}{
		{"target fits both budgets", tinyBudget(64, 64), true},
		{"target over the total", tinyBudget(64, int64(len(target))-1), false},
		{"target over the per-file bound", tinyBudget(int64(len(target))-1, 1024), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budgetParityCase(t, archive, root, members, tc.budget, tc.admit)
		})
	}
}

// TestManifestBudgetSymlinkCountsTowardTheRunningTotal: a symlink target's
// bytes share the file accumulator, so a file plus a target can cross together.
func TestManifestBudgetSymlinkCountsTowardTheRunningTotal(t *testing.T) {
	const target = "sibling.go"
	archive := budgetArchive(t,
		budgetTarFile(t, "a.go", 10),
		budgetTarSymlink(t, "nested/link", target),
	)
	fits := tinyBudget(64, int64(10+len(target)))
	if _, err := sourceManifestDigestFromArchiveWithBudget(archive, fits); err != nil {
		t.Fatalf("a file and a symlink target exactly filling the total must be admitted: %v", err)
	}
	_, err := sourceManifestDigestFromArchiveWithBudget(archive, tinyBudget(64, int64(10+len(target))-1))
	requireBudgetRefusal(t, "archive", err, "nested/link", "total")
}

// TestDefaultManifestBudgetClearsTheMeasuredMergeTree guards the shipped
// constants using the incident's measured sizes as literals, so it never
// asserts anything about the host's current tree.
func TestDefaultManifestBudgetClearsTheMeasuredMergeTree(t *testing.T) {
	const (
		observedMergedTreeBytes = int64(16_809_993) // refs/pull/830/merge
		observedMainTreeBytes   = int64(16_749_519) // main at the time
	)
	budget := defaultSourceManifestBudget()
	if budget.totalBytes <= observedMergedTreeBytes || budget.totalBytes <= observedMainTreeBytes {
		t.Fatalf("total budget %d does not clear the measured trees (%d merged, %d main)",
			budget.totalBytes, observedMergedTreeBytes, observedMainTreeBytes)
	}
	if budget.totalBytes < 2*observedMergedTreeBytes {
		t.Fatalf("total budget %d leaves less than double the measured merged tree %d",
			budget.totalBytes, observedMergedTreeBytes)
	}
	if budget.fileBytes <= 0 || budget.fileBytes >= budget.totalBytes {
		t.Fatalf("per-file budget %d must stay a bound under the total %d", budget.fileBytes, budget.totalBytes)
	}
	if budget.totalBytes <= 0 || maxHermeticSourceTransportBytes <= 0 ||
		maxHermeticSourceTransportMembers <= 0 || maxHermeticSourceTransportPathBytes <= 0 {
		t.Fatal("every manifest and transport bound must remain finite and positive")
	}
	if maxHermeticSourceTransportBytes <= budget.totalBytes {
		t.Fatalf("transport ceiling %d must exceed the manifest total %d",
			maxHermeticSourceTransportBytes, budget.totalBytes)
	}
}
