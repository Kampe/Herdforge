package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/standing"
)

func TestReviewSpawnPacketTargetsConfiguredSupervisor(t *testing.T) {
	repository, err := dispatch.AuthenticatedRepositoryIdentity(".")
	if err != nil {
		t.Fatalf("repository identity: %v", err)
	}
	for _, tc := range []struct {
		name, lane, role, ref string
	}{
		{name: "canonical", lane: "review-supervisor", role: "review-supervisor", ref: "FAC-SPAWN"},
		{name: "configured alias", lane: "assayer-chief", role: "review_harvest_supervisor", ref: "FAC-365"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packet := reviewSpawnPacket(&config.Config{Lanes: []config.LaneDef{
				{Name: tc.lane, Role: tc.role, Standing: true},
			}}, &provider.Task{Ref: tc.ref}, ".herd/worktrees/review", "go test ./pkg/dispatch")
			want := "REPORT_TARGET: " + standing.AgentNameForRepository(tc.lane, repository) + " (mandatory; never coordinator)"
			if !strings.Contains(packet, want) {
				t.Fatalf("spawn packet must target live configured supervisor %q:\n%s", want, packet)
			}
			if strings.Contains(packet, "REPORT_TARGET: coordinator") {
				t.Fatalf("spawn packet must never target coordinator:\n%s", packet)
			}
			if !strings.Contains(packet, "herd task verdict "+tc.ref+" APPROVED") || !strings.Contains(packet, "herd task verdict "+tc.ref+" REJECTED") {
				t.Fatalf("spawn packet must preserve typed verdict broker contract:\n%s", packet)
			}
		})
	}
}
