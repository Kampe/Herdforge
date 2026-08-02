package provider

import (
	"strconv"
	"strings"
)

// CompareRefs orders ticket refs with numeric awareness: FAC-9 < FAC-61 <
// FAC-100. Plain string comparison would put FAC-100 before FAC-99, breaking
// the Priority DESC, Ref ASC claim-order invariant. Non-conforming refs fall
// back to lexical comparison.
func CompareRefs(a, b string) int {
	ap, an, aok := splitRef(a)
	bp, bn, bok := splitRef(b)
	if aok && bok && strings.EqualFold(ap, bp) {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

func splitRef(ref string) (prefix string, num int, ok bool) {
	i := strings.LastIndex(ref, "-")
	if i <= 0 || i == len(ref)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return "", 0, false
	}
	return ref[:i], n, true
}
