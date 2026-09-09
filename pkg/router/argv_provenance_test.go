package router

import (
	"strings"
	"testing"
)

func TestProvenanceFromArgvReadsProviderAndModelBySearch(t *testing.T) {
	tests := []struct {
		name         string
		argv         []string
		wantProvider string
		wantModel    string
		wantErr      bool
	}{
		{name: "grok", argv: ArgvFor("grok", "grok-4.6", "medium"), wantProvider: "grok", wantModel: "grok-4.6"},
		{name: "codex", argv: ArgvFor("codex", "gpt-5.6-luna", "medium"), wantProvider: "codex", wantModel: "gpt-5.6-luna"},
		{name: "empty", argv: nil, wantErr: true},
		{name: "model-without-value", argv: []string{"grok", "--model"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, model, err := ProvenanceFromArgv(tt.argv)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if provider != tt.wantProvider || model != tt.wantModel {
				t.Fatalf("provenance = %s/%s, want %s/%s from argv %v", provider, model, tt.wantProvider, tt.wantModel, tt.argv)
			}
			if tt.wantModel != "" {
				found := false
				for i := range tt.argv {
					if tt.argv[i] == "--model" && i+1 < len(tt.argv) && tt.argv[i+1] == tt.wantModel {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("model %q not located by search in %v", tt.wantModel, tt.argv)
				}
			}
		})
	}
}

func TestFamilyForGrokArgvIsXAI(t *testing.T) {
	provider, model, err := ProvenanceFromArgv(ArgvFor("grok", "grok-4.6", "medium"))
	if err != nil {
		t.Fatal(err)
	}
	if got := FamilyFor(provider, model); got != "xai" {
		t.Fatalf("FamilyFor(%s,%s)=%q want xai", provider, model, got)
	}
	if strings.EqualFold(provider, "codex") {
		t.Fatal("grok argv collapsed to the first worker provider")
	}
}
