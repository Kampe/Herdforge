package resources

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScanReclaimable reports space recoverable WITHOUT deleting any unique work.
//
// FAC-654: the disk gate refused a worktree create with
// next_action=recover_capacity_without_cleanup and nothing else. That phrase
// tells an operator they are stuck; it does not tell them what to do. Measured
// during a live block: 5.0 GiB free against a 15 GiB reserve, with 19.5 GiB of
// rebuildable cache sitting on the same filesystem -- a dead pnpm store
// generation alone held 6.9 GiB while the active one held 66 MB. Dispatch was
// blocked for want of space that nothing needed.
//
// Worse, an operator staring at "below_threshold" reaches for the only large
// thing they can see, which is worktrees. That is exactly the wrong place: a
// worktree can hold uncommitted work or an unmerged branch, and deleting one to
// satisfy a gate trades correctness for capacity. This function deliberately
// reports ONLY caches -- compiler output and re-downloadable packages -- so the
// obvious action is also the safe one. Fleet state is never listed; releasing a
// worktree stays an explicit operator decision.
func ScanReclaimable(home string) []ReclaimableClass {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	type probe struct{ path, kind, rebuild string }
	probes := []probe{
		{filepath.Join(home, "Library", "Caches", "go-build"), "go build cache", "regenerated on next `go build`; `go clean -cache`"},
		{filepath.Join(home, ".cache", "go-build"), "go build cache", "regenerated on next `go build`; `go clean -cache`"},
		{filepath.Join(home, "go", "pkg", "mod"), "go module cache", "re-downloaded on next build; `go clean -modcache`"},
		{filepath.Join(home, ".npm", "_npx"), "npx package cache", "re-downloaded on next npx run"},
		{filepath.Join(home, ".cargo", "registry", "cache"), "cargo registry cache", "re-downloaded on next build"},
	}
	// Dead pnpm store generations: the active one is whichever version directory
	// pnpm currently writes, and every OLDER generation is pure garbage. This is
	// where the largest single win was found, and it is invisible to `pnpm store
	// prune`, which only prunes WITHIN the active generation.
	for _, base := range []string{
		filepath.Join(home, "Library", "pnpm", "store"),
		filepath.Join(home, ".local", "share", "pnpm", "store"),
	} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
				versions = append(versions, e.Name())
			}
		}
		if len(versions) < 2 {
			continue // only the active generation exists; nothing dead to report
		}
		sort.Slice(versions, func(i, j int) bool { return pnpmVersionNum(versions[i]) < pnpmVersionNum(versions[j]) })
		// Everything but the newest is a dead generation.
		for _, v := range versions[:len(versions)-1] {
			probes = append(probes, probe{filepath.Join(base, v), "dead pnpm store generation " + v, "superseded by the active store; safe to delete"})
		}
	}

	var out []ReclaimableClass
	for _, pr := range probes {
		n := dirBytes(pr.path)
		if n == 0 {
			continue
		}
		out = append(out, ReclaimableClass{Path: pr.path, Bytes: n, Kind: pr.kind, Rebuild: pr.rebuild})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

func pnpmVersionNum(name string) int {
	n := 0
	for _, r := range strings.TrimPrefix(name, "v") {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// maxScanFiles bounds the walk. A build cache holds hundreds of thousands of
// small files, and this runs inside a FAIL-CLOSED admission gate: a gate that
// takes minutes to explain itself is worse than one that explains less. The
// number only has to be good enough to rank classes and show an operator that
// gigabytes are sitting there, so it stops early and reports a floor.
const maxScanFiles = 20000

// dirBytes sums a directory subtree, bounded.
//
// A directory that cannot be walked reports what was readable rather than
// failing, and a subtree larger than the bound reports the bound's worth rather
// than walking it all. Both under-count, which is the safe direction: an
// under-count understates how much is reclaimable and can never talk an operator
// into deleting something on a promise the scan did not verify.
func dirBytes(root string) uint64 {
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return 0
	}
	var total uint64
	var seen int
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		seen++
		if seen > maxScanFiles {
			return filepath.SkipAll
		}
		if info, statErr := d.Info(); statErr == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}
