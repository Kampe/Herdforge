package verifier

import (
	"testing"
	"time"
)

func TestApplyTestTimeout(t *testing.T) {
	tests := []struct {
		name, command, want string
	}{
		{name: "go test gets configured timeout", command: "go test ./...", want: "go test -timeout=30m0s ./..."},
		{name: "existing timeout is preserved", command: "go test -timeout=45m ./...", want: "go test -timeout=45m ./..."},
		{name: "split timeout is preserved", command: "go test -timeout 45m ./...", want: "go test -timeout 45m ./..."},
		{name: "non go command is unchanged", command: "npm test", want: "npm test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyTestTimeout(tt.command, 30*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("ApplyTestTimeout(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

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
