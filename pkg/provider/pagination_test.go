package provider

import (
	"fmt"
	"testing"
)

func TestDecidePagination_EmptyNotShort(t *testing.T) {
	// Short page with fresh items MUST continue (server may cap below pageSize).
	if d := DecidePagination(5, 5); d != PageContinue {
		t.Fatalf("short page with fresh=5: got %v want PageContinue", d)
	}
	// Full page continues.
	if d := DecidePagination(100, 100); d != PageContinue {
		t.Fatalf("full page: got %v want PageContinue", d)
	}
	// Empty page terminates.
	if d := DecidePagination(0, 0); d != PageStopEmpty {
		t.Fatalf("empty page: got %v want PageStopEmpty", d)
	}
	// Non-empty page with zero fresh IDs = duplicate page.
	if d := DecidePagination(100, 0); d != PageStopDuplicate {
		t.Fatalf("duplicate page: got %v want PageStopDuplicate", d)
	}
	// Non-vacuity: if we wrongly terminated on short page, a board with a
	// server-capped page of 99 would hide the tail. Prove short != empty.
	if DecidePagination(99, 99) == PageStopEmpty {
		t.Fatal("short page must not equal empty-page termination")
	}
}

func TestPageAccumulator_MultiPage(t *testing.T) {
	acc := NewPageAccumulator()

	// Page 1: 100 items (simulating a full/capped page).
	page1 := make([]string, 100)
	for i := range page1 {
		page1[i] = fmt.Sprintf("id-%d", i+1)
	}
	fresh, dec := acc.IngestPage(page1)
	if fresh != 100 || dec != PageContinue {
		t.Fatalf("page1: fresh=%d dec=%v", fresh, dec)
	}

	// Page 2: short tail of 5 — must continue signal (caller still requests page 3).
	page2 := []string{"id-101", "id-102", "id-103", "id-104", "id-105"}
	fresh, dec = acc.IngestPage(page2)
	if fresh != 5 || dec != PageContinue {
		t.Fatalf("page2 short: fresh=%d dec=%v want continue", fresh, dec)
	}
	if acc.Len() != 105 {
		t.Fatalf("len=%d want 105", acc.Len())
	}

	// Page 3: empty — stop.
	fresh, dec = acc.IngestPage(nil)
	if fresh != 0 || dec != PageStopEmpty {
		t.Fatalf("page3 empty: fresh=%d dec=%v", fresh, dec)
	}

	// Duplicate page detection: re-feed page1.
	fresh, dec = acc.IngestPage(page1)
	if fresh != 0 || dec != PageStopDuplicate {
		t.Fatalf("duplicate: fresh=%d dec=%v", fresh, dec)
	}

	ids := acc.IDs()
	if len(ids) != 105 || ids[0] != "id-1" || ids[104] != "id-105" {
		t.Fatalf("ids order broken: len=%d first=%v last=%v", len(ids), ids[0], ids[len(ids)-1])
	}
}

func TestPageAccumulator_EmptyFirstPage(t *testing.T) {
	acc := NewPageAccumulator()
	fresh, dec := acc.IngestPage([]string{})
	if fresh != 0 || dec != PageStopEmpty {
		t.Fatalf("empty first page: fresh=%d dec=%v", fresh, dec)
	}
	if acc.Len() != 0 {
		t.Fatalf("len=%d want 0 (empty legitimate page, not failure)", acc.Len())
	}
}
