package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/security"
)

func TestProductionControlFactoryScopesSequentialIdentities(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	d := dispatch.NewProductionDispatcher(nil, nil, nil)
	closeControl, err := configureProductionControl(d, root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	a := control.LaneIdentity{Repository: "repo", TaskRef: "FAC-A", Lane: "worker-a", LeaseGeneration: 1, CandidateSHA: "sha-a"}
	b := control.LaneIdentity{Repository: "repo", TaskRef: "FAC-B", Lane: "worker-b", LeaseGeneration: 2, CandidateSHA: "sha-b"}
	oA, err := d.ControlFactory(context.Background(), dispatch.ControlScope{Identity: a, Wake: control.WakeTarget{Target: "a", TabID: "ta", PaneID: "pa", AgentName: "a", LeaseGeneration: 1}, Check: func(context.Context, control.Order) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	oB, err := d.ControlFactory(context.Background(), dispatch.ControlScope{Identity: b, Wake: control.WakeTarget{Target: "b", TabID: "tb", PaneID: "pb", AgentName: "b", LeaseGeneration: 2}, Check: func(context.Context, control.Order) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if oA.Identity == oB.Identity || oA.Delivery.Owner == oB.Delivery.Owner {
		t.Fatal("sequential tasks reused identity or owner")
	}
	_, err = oA.Delivery.Deliver(context.Background(), control.Order{LaneIdentity: b, Kind: control.KindRepair, Body: "wrong task"})
	if !errors.Is(err, control.ErrStaleIdentity) {
		t.Fatalf("stale sequential task reached sender: %v", err)
	}
}

func TestConfigureProductionControlBootstrapsDurableMACSecret(t *testing.T) {
	t.Setenv("HERD_CONTROL_SECRET", "")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := dispatch.NewProductionDispatcher(nil, nil, nil)
	closeControl, err := configureProductionControl(d, root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	if d.Control == nil || strings.TrimSpace(d.Control.Secret) == "" {
		t.Fatal("coordinator startup must load a durable MAC secret without env")
	}
	path := security.ControlMACSecretPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("durable mac.secret missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", fi.Mode().Perm())
	}
	reload := dispatch.NewProductionDispatcher(nil, nil, nil)
	closeReload, err := configureProductionControl(reload, root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeReload()
	if reload.Control.Secret != d.Control.Secret {
		t.Fatal("restart must drain the same durable secret")
	}
}

func TestConfigureProductionControlEnvMismatchFailClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := security.WriteControlMACSecret(root, "durable-coordinator-mac-secret"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CONTROL_SECRET", "other-coordinator-mac-secret")
	d := dispatch.NewProductionDispatcher(nil, nil, nil)
	if _, err := configureProductionControl(d, root); err == nil {
		t.Fatal("mismatched env secret must fail closed")
	}
	got, err := security.ReadControlMACSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "durable-coordinator-mac-secret" {
		t.Fatal("mismatch must preserve the durable secret")
	}
}

func TestCoordinatorControlMACSecretRefusesWorkerRole(t *testing.T) {
	root := t.TempDir()
	_, _, err := security.BootstrapOrLoadControlMACSecret(root, "", "worker")
	if err == nil {
		t.Fatal("worker must not bootstrap the control MAC secret")
	}
	if _, statErr := os.Lstat(security.ControlMACSecretPath(root)); !os.IsNotExist(statErr) {
		t.Fatal("worker bootstrap must not create mac.secret")
	}
}

func TestConfigureProductionControlRejectsEscapingMailPath(t *testing.T) {
	for _, mailPath := range []string{"../mail.jsonl", "/tmp/mail.jsonl"} {
		t.Run(mailPath, func(t *testing.T) {
			t.Setenv("HERD_CONTROL_SECRET", "test-control-secret")
			t.Setenv("HERD_MAIL_FILE", mailPath)
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
				t.Fatal(err)
			}
			d := dispatch.NewProductionDispatcher(nil, nil, nil)
			if _, err := configureProductionControl(d, root); err == nil {
				t.Fatal("expected escaping mailbox path rejection")
			}
		})
	}
}

func TestConfigureProductionControlAcceptsRelativeMailPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CONTROL_SECRET", "test-control-secret")
	t.Setenv("HERD_MAIL_FILE", "nested/mail.jsonl")
	d := dispatch.NewProductionDispatcher(nil, nil, nil)
	closeControl, err := configureProductionControl(d, root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	if d.Control == nil || d.Control.Mailbox == nil {
		t.Fatal("control mailbox was not configured")
	}
}

func TestCoordinatorControlReconcilerDoesNotRequireTaskTabGeneration(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	loop, closeControl, err := newCoordinatorControlReconciler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	// A coordinator has no managed task-tab lease. An empty durable outbox is
	// still a real reconciliation pass and must not consult Herdr or invent a
	// generation merely to make startup appear composed.
	if err := loop.RunOnce(context.Background()); err != nil {
		t.Fatalf("coordinator reconciliation: %v", err)
	}
}
