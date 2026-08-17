package reviewledger

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func driftConfig() *config.Config {
	return &config.Config{Lanes: []config.LaneDef{
		{Name: "coordinator", Standing: true},
		{Name: "worker", Standing: false},
		{Name: "harvest", Standing: true},
	}}
}

func TestCompareStandingBuilderFamiliesReportsOnlyMismatches(t *testing.T) {
	rows := []LedgerRow{
		{Event: string(EventRecord), BuilderIdentity: "forge-coordinator", BuilderFamily: "anthropic"},
		{Event: string(EventRecord), BuilderIdentity: "forge-harvest", BuilderFamily: "openai"},
		{Event: string(EventRecord), BuilderIdentity: "forge-worker", BuilderFamily: "anthropic"},
	}
	live := []LiveBuilder{
		{Identity: "forge-coordinator", Family: "openai"},
		{Identity: "forge-harvest", Family: "openai"},
		{Identity: "forge-worker", Family: "openai"},
	}
	got, err := CompareStandingBuilderFamilies(driftConfig(), rows, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("findings=%+v, want exactly the coordinator mismatch", got)
	}
	if got[0] != (BuilderFamilyDrift{Lane: "coordinator", Identity: "forge-coordinator", Recorded: "anthropic", Live: "openai"}) {
		t.Fatalf("finding=%+v", got[0])
	}
}

func TestCompareStandingBuilderFamiliesUsesLatestRecordAndRejectsAmbiguity(t *testing.T) {
	rows := []LedgerRow{
		{Event: string(EventRecord), BuilderIdentity: "forge-coordinator", BuilderFamily: "anthropic"},
		{Event: string(EventRecord), BuilderIdentity: "forge-coordinator", BuilderFamily: "openai"},
	}
	got, err := CompareStandingBuilderFamilies(driftConfig(), rows, []LiveBuilder{{Identity: "forge-coordinator", Family: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("latest matching record must be silent, got %+v", got)
	}
	_, err = CompareStandingBuilderFamilies(driftConfig(), rows, []LiveBuilder{
		{Identity: "forge-coordinator", Family: "openai"},
		{Identity: "forge-coordinator", Family: "anthropic"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate live builder identity") {
		t.Fatalf("ambiguous inventory must fail closed, got %v", err)
	}
}
