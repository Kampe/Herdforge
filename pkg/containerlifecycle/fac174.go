package containerlifecycle

import "sort"

// FAC174LegacyBaseline is the exact, full container IDs of the 18
// leftover FAC-174 containers observed live on 2026-08-04 (17 Exited, 1
// Created) before this package's receipt store existed — none of them
// were ever registered, so AuditUnowned correctly reports them as
// unowned, but that alone doesn't distinguish "known baseline, already
// reviewed" from "new straggler that showed up after this landed". See
// docs/fac-200-fac174-reconciliation-plan.md for the supervised removal
// procedure this baseline is scoped to. This is data for
// LabelFAC174Baseline's read-only categorization — never a removal
// target list.
var FAC174LegacyBaseline = []string{
	"18381a40355cf1333b45c51862b1d3ad16976126a233c8fbcba107571a267ef6",
	"8fcf80b37d6dcb11925efbbe7cf7e4c6dba2c5f1fc987fc9aca6ca0b4b3f4b5a",
	"b830c3b4d07facc23f6a472516e90004e258883db5087f75c8a5fa162c8275a9",
	"3468570c2d35d61d727332ce023adc9bbf3fb2d3f5bc342500fa89ad99ec81c4",
	"784ccb775c84d6b1aeb4fbbddd94e6e387a32bcd91b5c2fd7b0f62cd7399ae2c",
	"432dedda3cd03b1c3d5ca52afe87784c426d1f0f47afbcbf6020f09758c187bb",
	"92323e57cb7221cc226bc28499985e9a0a4e4707dfaaf63ec786c52d5f320039",
	"d8b77814ea9deeaa74f2783462df3896ea52e523d4136faa8f8380a62d92ae77",
	"8f8bd32b79c99995f4a6217601fe1895d562ccd3df64bbb73b8f9fd940ac5ca3",
	"ad8f9ad68d0667e14367cfac804c3ec39c399aa3d9711407af04a570a3e0a7fb",
	"6a282ab14e34693c3f871b012bd4eadedb2ce8ada6bf10ca39a8a763ee0472a6",
	"ebad9ea54f615be75d31427647a43b1f3b70c05bd1d3b22a5a6a4a59a9df9361",
	"9ca6e49e0c84fefb9a8f9c9469246166e75b328ac6791c390da6438087d30327",
	"7b1d9e97cf34b08e14e6ea14d7320282ccde6fb39ae1b812bb3ea82f296935a9",
	"afffce0037d621772ac544c4f9bde3019e3b33d702f7213e8f9294f48f997c0a",
	"f707d26f93879fc61c8648e3518eee52ae94dc75cdc0b6de10dfc654967c0e4e",
	"b78ff8cc262c709cf85d478f6ada32b385da86bd4e4743fd0a7b2ba05003e15c",
	"393cfa606510820f5338733990136b0f906fa5a35d6b98f870a4fdec5de1c81e",
}

func isFAC174Baseline(id string) bool {
	for _, b := range FAC174LegacyBaseline {
		if b == id {
			return true
		}
	}
	return false
}

// LabelFAC174Baseline splits an AuditUnowned/Status result into the
// known FAC-174 baseline (already inventoried; see the reconciliation
// plan doc) and everything else (a new unowned straggler that needs its
// own investigation, not blanket coverage by the FAC-174 plan). Both
// return values are sorted by ID and neither is ever removed here.
func LabelFAC174Baseline(unowned []LiveContainer) (baseline, other []LiveContainer) {
	for _, c := range unowned {
		if isFAC174Baseline(c.ID) {
			baseline = append(baseline, c)
		} else {
			other = append(other, c)
		}
	}
	sort.Slice(baseline, func(i, j int) bool { return baseline[i].ID < baseline[j].ID })
	sort.Slice(other, func(i, j int) bool { return other[i].ID < other[j].ID })
	return baseline, other
}
