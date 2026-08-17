package main

import "testing"

func TestProductionModeIsExplicit(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		secret   string
		wantProd bool
	}{
		{name: "unset is local", wantProd: false},
		{name: "local", mode: "local", wantProd: false},
		{name: "production", mode: "production", wantProd: true},
		{name: "control secret opts in", secret: "control", wantProd: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HERD_MODE", tt.mode)
			t.Setenv("HERD_CONTROL_SECRET", tt.secret)
			if got := productionMode(); got != tt.wantProd {
				t.Fatalf("productionMode()=%v, want %v", got, tt.wantProd)
			}
		})
	}
}
