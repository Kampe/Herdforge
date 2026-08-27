package review

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

func TestTipIndexAfterResumesPastCursor(t *testing.T) {
	tips := []harvest.UnmergedWork{
		{Unmerged: []string{"aaa"}},
		{Unmerged: []string{"bbb"}},
		{Unmerged: []string{"ccc"}},
	}
	if got := tipIndexAfter(tips, "bbb"); got != 2 {
		t.Fatalf("after bbb: got %d want 2", got)
	}
	if got := tipIndexAfter(tips, ""); got != 0 {
		t.Fatalf("empty cursor must restart: got %d", got)
	}
	if got := tipIndexAfter(tips, "missing"); got != 0 {
		t.Fatalf("stale cursor must restart: got %d", got)
	}
	if got := tipIndexAfter(tips, "ccc"); got != 3 {
		t.Fatalf("after last tip: got %d want 3 (nothing left)", got)
	}
}
