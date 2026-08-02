package sync

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// mentionPivot is the verbatim zsh mention-keyword list from bin/herd-board-sync
// line 97: a ref in a commit SUBJECT counts as shipped only when it is NOT
// mention-preceded ("... after CHA-476", "R2 follow-up on CHA-268"). The regex
// matches the keyword, then up to 15 lowercase chars, then the ref itself.
const mentionPivot = `\b(after|before|follow-?up|prep(aration)?|refs?|references?|see|per|towards?|related( to)?|unblocks?|blocked by|depends on|part of|child of|parent of|split from|superseded by|replaces)\b[ a-z]{0,15}\b`

// EpochString converts an ISO8601 timestamp (e.g. 2026-07-24T09:04:22.428Z) to a
// unix epoch, porting bin/herd-board-sync's _iso_to_epoch: strip fractional
// seconds and trailing Z, then parse. Returns (0, false) on failure, which
// disables the createdAt date gate for that ticket (degrades safely to
// subject+mention matching).
func EpochString(iso string) (int64, bool) {
	iso = strings.TrimSuffix(strings.Split(iso, ".")[0], "Z")
	if iso == "" {
		return 0, false
	}
	t, err := time.Parse("2006-01-02T15:04:05", iso)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// RefShipped decides whether a merged commit log proves ref shipped. Port of
// _bsync_ref_shipped (bin/herd-board-sync lines 78-102): a ref counts as
// SHIPPED only when a merged commit satisfies ALL of
//   - the ref appears in the commit SUBJECT (log is subjects-only %s, so a
//     body-only mention never counts);
//   - that subject occurrence is NOT mention-preceded;
//   - the commit is NOT OLDER than createdEpoch (kills ref-reuse across a
//     board rollback that re-minted CHA-nnn refs). createdEpoch<=0 disables
//     only the date gate.
// Log lines are "epoch<TAB>subject"; empty subjects are skipped.
func RefShipped(log, ref string, createdEpoch int64) bool {
	ref = strings.ToLower(ref)
	wordBoundary := regexp.MustCompile(`\b` + regexp.QuoteMeta(ref) + `\b`)
	mention := regexp.MustCompile(mentionPivot + `\b` + regexp.QuoteMeta(ref) + `\b`)
	for _, line := range strings.Split(strings.TrimSuffix(log, "\n"), "\n") {
		if line == "" {
			continue
		}
		tsStr, subj, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(subj) == "" {
			continue
		}
		if !wordBoundary.MatchString(subj) {
			continue
		}
		if mention.MatchString(subj) {
			continue
		}
		if createdEpoch > 0 {
			if ts, err := strconv.ParseInt(strings.TrimSpace(tsStr), 10, 64); err == nil && ts > 0 && ts < createdEpoch {
				continue
			}
		}
		return true
	}
	return false
}