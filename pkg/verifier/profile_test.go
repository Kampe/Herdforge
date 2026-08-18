package verifier

import "testing"

func TestCommandProfileMatchesConfiguredCommands(t *testing.T) {
	profile := CommandProfile{
		ID:               "config-verification",
		BuildCommand:     "go build ./...",
		TestCommand:      "go test ./...",
		PreflightCommand: "go vet ./...",
	}
	tests := []struct {
		name                   string
		build, test, preflight string
		want                   bool
	}{
		{name: "configured profile", build: "go build ./...", test: "go test ./...", preflight: "go vet ./...", want: true},
		{name: "vacuous test override", build: "go build ./...", test: "true", preflight: "go vet ./..."},
		{name: "build override", build: "true", test: "go test ./...", preflight: "go vet ./..."},
		{name: "preflight override", build: "go build ./...", test: "go test ./...", preflight: "true"},
		{name: "whitespace is normalized", build: " go build ./... ", test: "go test ./...", preflight: "go vet ./...", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profile.Matches(tt.build, tt.test, tt.preflight); got != tt.want {
				t.Fatalf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandProfileDigestChangesWithCommands(t *testing.T) {
	profile := CommandProfile{ID: "config-verification", BuildCommand: "go build ./...", TestCommand: "go test ./..."}
	if got := profile.Digest(); len(got) != len("sha256:")+64 {
		t.Fatalf("digest = %q, want a full sha256 digest", got)
	}
	changed := profile
	changed.TestCommand = "true"
	if changed.Digest() == profile.Digest() {
		t.Fatal("command profile digest must change when a command changes")
	}
}
