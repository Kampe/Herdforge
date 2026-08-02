package sync

import (
	"context"
	"testing"
)

func TestReconcileBoard_NilProvider(t *testing.T) {
	syncer := NewBoardSyncer(nil)
	_, err := syncer.ReconcileBoard(context.Background(), "proj-1", t.TempDir())
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}
