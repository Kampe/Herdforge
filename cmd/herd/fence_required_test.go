package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// fenceRequiringProvider mimics the live Kaneo adapter: it refuses any status
// mutation that arrives without op/fence meta, exactly as FAC-147 requires.
type fenceRequiringProvider struct {
	*provider.MemoryProvider
	unfencedAttempts int
}

func (p *fenceRequiringProvider) UpdateStatus(ctx context.Context, taskID, status string) error {
	p.unfencedAttempts++
	return errUnfenced
}

type unfencedError struct{}

func (unfencedError) Error() string {
	return "kaneo: mutation refused without X-Herd-Op (unfenced bypass; FAC-147 fail-closed)"
}

var errUnfenced = unfencedError{}

// TestOverrideNeverWritesUnfenced is the FAC-566 regression for the second
// defect.
//
// FAC-563 routed missing-receipt overrides through the plain provider, and the
// live Kaneo adapter correctly refused: "mutation refused without X-Herd-Op".
// That refusal was right and my design was wrong -- I had conflated the
// completion RECEIPT with the mutation FENCE. An override replaces proof that
// the work landed; it never replaces the coordinator's authority to write.
//
// With no claim stack there is no fence, so the close must refuse BEFORE
// touching the provider. If it ever reaches an unfenced UpdateStatus, that is a
// real fail-open and this test fails.
func TestOverrideNeverWritesUnfenced(t *testing.T) {
	cfg, mp, root := overrideFixture(t)
	fenced := &fenceRequiringProvider{MemoryProvider: mp}

	_, err := approveByOverrideWithAcceptance(context.Background(), cfg, fenced, nil, root, "CHA-2165",
		"$ cd packages/adapters && pnpm test\nok 42 passed\n",
		&hsync.OverrideRequest{
			Policy:   "operator-external-merge",
			Actor:    "coordinator",
			Reason:   "landed outside the fleet path",
			Evidence: "cross-family PASS ingested; activation proof",
		})
	if err == nil {
		t.Fatal("an override with no fence must refuse rather than write")
	}
	if fenced.unfencedAttempts != 0 {
		t.Fatalf("override attempted %d UNFENCED provider write(s); it must never reach the provider without a fence",
			fenced.unfencedAttempts)
	}
	if !strings.Contains(err.Error(), "fence") && !strings.Contains(err.Error(), "FAC-147") {
		t.Fatalf("refusal must name the fence requirement, got %v", err)
	}
}
