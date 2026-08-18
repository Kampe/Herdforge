package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestFindReviewSupervisorLane(t *testing.T) {
	tests := []struct {
		name string
		role string
	}{
		{name: "canonical role", role: "review-supervisor"},
		{name: "hyphenated role", role: "review-harvest-supervisor"},
		{name: "legacy underscore role", role: "review_harvest_supervisor"},
		{name: "legacy harvest role", role: "harvest-supervisor"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lane := findReviewSupervisorLane(&config.Config{Lanes: []config.LaneDef{{
				Name: "standing-review", Role: tt.role,
			}}})
			if lane == nil {
				t.Fatalf("findReviewSupervisorLane did not resolve role %q", tt.role)
			}
			if lane.Role != tt.role {
				t.Fatalf("resolved role = %q, want %q", lane.Role, tt.role)
			}
		})
	}
}
