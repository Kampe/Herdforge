package main

import (
	"strings"
	"testing"
)

func TestRequireServingBrokerRefusesUnavailableBroker(t *testing.T) {
	err := requireServingBroker(t.TempDir(), t.TempDir()+"/missing.sock")
	if err == nil {
		t.Fatal("missing broker must be refused")
	}
	if !strings.Contains(err.Error(), "broker unavailable") {
		t.Fatalf("error=%v, want broker unavailable", err)
	}
}
