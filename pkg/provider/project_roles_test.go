package provider

import (
	"strings"
	"testing"
)

// TestProjectRoleIsRecognized is the FAC-567 regression.
//
// A card whose only label was "backend-api" -- a canonical implementation lane
// in the consumer's repository -- was refused with "no recognized
// implementation role in labels [backend-api]", so the newly legitimate fence
// could not be minted for a project role.
func TestProjectRoleIsRecognized(t *testing.T) {
	t.Cleanup(func() { RegisterProjectImplementationRoles(nil) })

	task := &Task{ID: "t1", Ref: "CHA-2183", Labels: []string{"backend-api"}}

	if _, err := TaskOwnershipRole(task, "worker"); err == nil {
		t.Fatal("fixture must start from the failing state: backend-api unregistered")
	}

	RegisterProjectImplementationRoles([]string{"backend-api", "chain-indexer"})
	got, err := TaskOwnershipRole(task, "worker")
	if err != nil {
		t.Fatalf("a registered project role must be accepted: %v", err)
	}
	if got != "backend-api" {
		t.Fatalf("ownership role = %q, want the project label", got)
	}
}

// Registration EXTENDS the vocabulary; the generic roles must keep working.
func TestGenericRolesSurviveRegistration(t *testing.T) {
	t.Cleanup(func() { RegisterProjectImplementationRoles(nil) })
	RegisterProjectImplementationRoles([]string{"backend-api"})

	for _, generic := range []string{"forge-smith", "worker", "builder", "coder"} {
		task := &Task{ID: "t", Labels: []string{generic}}
		if _, err := TaskOwnershipRole(task, ""); err != nil {
			t.Fatalf("generic role %q must still resolve: %v", generic, err)
		}
	}
}

// An unknown label still refuses. Widening recognition must not become
// accepting anything, or a typo could claim ownership.
func TestUnknownLabelStillRefuses(t *testing.T) {
	t.Cleanup(func() { RegisterProjectImplementationRoles(nil) })
	RegisterProjectImplementationRoles([]string{"backend-api"})

	task := &Task{ID: "t", Labels: []string{"backend-apy"}}
	err := TaskOwnershipRole2(task)
	if err == nil {
		t.Fatal("an unregistered label must still refuse")
	}
	// The message must list what IS accepted, including project roles, so an
	// operator can see the vocabulary rather than guess it.
	if !strings.Contains(err.Error(), "backend-api") {
		t.Fatalf("error must show the effective vocabulary, got %v", err)
	}
}

// TaskOwnershipRole2 is a test helper returning only the error.
func TaskOwnershipRole2(task *Task) error {
	_, err := TaskOwnershipRole(task, "")
	return err
}

// Clearing restores the generic-only vocabulary.
func TestClearingRegistrationRestoresGenericOnly(t *testing.T) {
	RegisterProjectImplementationRoles([]string{"backend-api"})
	RegisterProjectImplementationRoles(nil)
	if _, err := TaskOwnershipRole(&Task{ID: "t", Labels: []string{"backend-api"}}, ""); err == nil {
		t.Fatal("cleared registration must no longer accept the project role")
	}
	if len(KnownImplementationRoles()) != 4 {
		t.Fatalf("cleared vocabulary must be the four generic roles, got %v", KnownImplementationRoles())
	}
}
