package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
)

func TestReviewSpawnPacketTargetsConfiguredSupervisor(t *testing.T) {
	packet := reviewSpawnPacket(&config.Config{Lanes: []config.LaneDef{
		{Name: "review-supervisor", Role: "review-supervisor"},
	}}, &provider.Task{Ref: "FAC-SPAWN"}, ".herd/worktrees/review", "go test ./pkg/dispatch")

	if !strings.Contains(packet, "REPORT_TARGET: forge-review-supervisor (mandatory; never coordinator)") {
		t.Fatalf("spawn packet must target configured supervisor:\n%s", packet)
	}
	if strings.Contains(packet, "REPORT_TARGET: review-harvest-supervisor") || strings.Contains(packet, "REPORT_TARGET: coordinator") {
		t.Fatalf("spawn packet must not target legacy supervisor or coordinator:\n%s", packet)
	}
	if !strings.Contains(packet, "herd task verdict FAC-SPAWN APPROVED") || !strings.Contains(packet, "herd task verdict FAC-SPAWN REJECTED") {
		t.Fatalf("spawn packet must preserve typed verdict broker contract:\n%s", packet)
	}
}

func TestReviewSpawnPacketTargetsConfiguredSupervisorAlias(t *testing.T) {
	packet := reviewSpawnPacket(&config.Config{Lanes: []config.LaneDef{
		{Name: "assayer-chief", Role: "review_harvest_supervisor", Standing: true},
	}}, &provider.Task{Ref: "FAC-365"}, ".herd/worktrees/review", "go test ./pkg/dispatch")
	if !strings.Contains(packet, "REPORT_TARGET: forge-assayer-chief (mandatory; never coordinator)") {
		t.Fatalf("spawn packet must target configured supervisor alias:\n%s", packet)
	}
}

func TestCanonicalRouterRoleAcceptsSupervisorAliases(t *testing.T) {
	for _, raw := range []string{"review_harvest_supervisor", "harvest-supervisor", " REVIEW-SUPERVISOR "} {
		if got := canonicalRouterRole(raw); got != router.RoleReviewSupervisor {
			t.Fatalf("canonicalRouterRole(%q)=%q, want %q", raw, got, router.RoleReviewSupervisor)
		}
	}
}
