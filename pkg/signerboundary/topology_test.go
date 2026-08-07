package signerboundary

import (
	"os"
	"testing"
)

func TestTopology_RequiresThreeDistinctUIDs(t *testing.T) {
	t.Setenv(EnvSocketGID, "20")
	t.Setenv(EnvSignerUID, "10")
	t.Setenv(EnvRequesterUID, "10") // same as signer
	t.Setenv(EnvBuilderUID, "12")
	if _, err := RequireTopology(); err == nil {
		t.Fatal("S==R must fail")
	}
	t.Setenv(EnvSignerUID, "10")
	t.Setenv(EnvRequesterUID, "11")
	t.Setenv(EnvBuilderUID, "11") // same as requester
	if _, err := RequireTopology(); err == nil {
		t.Fatal("R==B must fail — worker would share requester identity")
	}
}

func TestTopology_ProcessMustBeRequester(t *testing.T) {
	t.Setenv(EnvSocketGID, "20")
	t.Setenv(EnvSignerUID, "10")
	t.Setenv(EnvRequesterUID, "99999") // not us
	t.Setenv(EnvBuilderUID, "12")
	if _, err := RequireTopology(); err == nil {
		t.Fatal("process must run as requester uid")
	}
}

func TestAuthorizePeerUID_ExcludesBuilder(t *testing.T) {
	topo := Topology{SignerUID: 1, RequesterUID: 2, BuilderUID: 3}
	if err := AuthorizePeerUID(2, topo); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizePeerUID(3, topo); err == nil {
		t.Fatal("builder must be denied")
	}
	if err := AuthorizePeerUID(1, topo); err == nil {
		t.Fatal("signer must be denied as peer")
	}
	if err := AuthorizePeerUID(99, topo); err == nil {
		t.Fatal("unknown uid must be denied")
	}
}

// Hostile same-UID scenario: worker uid equals requester uid means peer-cred
// cannot exclude the worker. Topology refuses R==B.
func TestHostileSameUID_TopologyRefusesBuilderAsRequester(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvSocketGID, itoa(os.Getgid()))
	t.Setenv(EnvSignerUID, "1")
	if me == 1 {
		t.Setenv(EnvSignerUID, "2")
	}
	t.Setenv(EnvRequesterUID, itoa(me))
	t.Setenv(EnvBuilderUID, itoa(me)) // same — hostile topology
	if _, err := RequireTopology(); err == nil {
		t.Fatal("builder uid must not equal requester uid")
	}
}
