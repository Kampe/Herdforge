package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestReportStandingBuilderFamilyDriftSilentForAgreement(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "coordinator", Standing: true}}}
	rows := []reviewledger.LedgerRow{{
		Event: string(reviewledger.EventRecord), BuilderIdentity: "forge-coordinator", BuilderFamily: "anthropic",
	}}
	findings, err := reportStandingBuilderFamilyDrift(cfg, rows, func() ([]herdr.AgentEntry, error) {
		return []herdr.AgentEntry{{Name: "forge-coordinator", Kind: "claude"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("agreement must be silent, findings=%+v", findings)
	}
}

func TestReportStandingBuilderFamilyDriftFailsClosedOnUnreadableInventory(t *testing.T) {
	called := false
	_, err := reportStandingBuilderFamilyDrift(&config.Config{}, nil, func() ([]herdr.AgentEntry, error) {
		called = true
		return nil, errors.New("fixture inventory unavailable")
	})
	if !called {
		t.Fatal("inventory reader was not called")
	}
	if err == nil || !strings.Contains(err.Error(), "live agent inventory unavailable") {
		t.Fatalf("unreadable inventory must fail closed, got %v", err)
	}
}
