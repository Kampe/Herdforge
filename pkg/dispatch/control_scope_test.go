package dispatch

import (
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/security"
)

func TestLaunchControlScope_FailClosedUnknownPackages(t *testing.T) {
	// Nil / empty policy packages must BLOCK — never invent FAC-133 packages.
	if _, err := launchControlScope(nil, "wt"); err == nil {
		t.Fatal("nil policy must fail closed")
	}
	p := &security.LaunchPolicy{}
	_, err := launchControlScope(p, "wt")
	if err == nil {
		t.Fatal("empty PackageAllowlist must fail closed")
	}
	if !errors.Is(err, security.ErrUnknownPolicy) {
		t.Fatalf("want ErrUnknownPolicy, got %v", err)
	}
	p.PackageAllowlist = []string{"pkg/security", "cmd/herd"}
	sc, err := launchControlScope(p, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Exclusive || len(sc.PackageAllowlist) != 2 {
		t.Fatalf("%+v", sc)
	}
	// Must not silently expand to hardcoded three-package set.
	if len(sc.PackageAllowlist) != 2 || sc.PackageAllowlist[1] != "cmd/herd" {
		t.Fatalf("unexpected packages: %+v", sc.PackageAllowlist)
	}
}

func TestIssuePreLaunchControl_RefusesPending(t *testing.T) {
	cp := testControlPlane(t)
	d := &Dispatcher{Control: cp}
	p := &security.LaunchPolicy{PackageAllowlist: []string{"pkg/envelope"}}
	_, err := d.IssuePreLaunchControl("pending-FAC-133-1", "FAC-133", 1, "wt", p)
	if err == nil {
		t.Fatal("pending-* must be refused")
	}
	ctrl, err := d.IssuePreLaunchControl("ses_live", "FAC-133", 1, "wt", p)
	if err != nil {
		t.Fatal(err)
	}
	if ctrl == nil || ctrl.TargetWorkerSession != "ses_live" {
		t.Fatalf("%+v", ctrl)
	}
}
