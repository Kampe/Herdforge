package main

import "testing"

func TestSafeReviewSurfacePart(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "ticket", ref: "FAC-435", want: "fac-435"},
		{name: "slashes and punctuation", ref: "review/CHA-12#r2", want: "review-cha-12-r2"},
		{name: "empty", ref: "!!!", want: "candidate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeReviewSurfacePart(tt.ref); got != tt.want {
				t.Fatalf("safeReviewSurfacePart(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestReviewPoolMode(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"FAC-1", "--pool"}, want: true},
		{args: []string{"--pool=true", "FAC-1"}, want: true},
		{args: []string{"FAC-1", "--spawn"}, want: false},
	} {
		if got := reviewPoolMode(tt.args); got != tt.want {
			t.Fatalf("reviewPoolMode(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
