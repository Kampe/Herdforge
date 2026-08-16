package provider

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestNewFromHerdConfigUsesExplicitKaneoEnvironmentOrigin(t *testing.T) {
	t.Setenv("KANEO_API_URL", "https://kaneo.example.test")
	cfg := &config.Config{TaskProvider: config.TaskProvider{Type: "kaneo", Enabled: []string{"kaneo"}, ProjectID: "project"}}
	tp, err := NewFromHerdConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := UnwrapTaskProvider(tp).(*KaneoProvider)
	if !ok || k.APIURL != "https://kaneo.example.test" || !k.UseCLI {
		t.Fatalf("Kaneo provider origin=%q use_cli=%v, want profile-backed CLI", k.APIURL, k.UseCLI)
	}
}
