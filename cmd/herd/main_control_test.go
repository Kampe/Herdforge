package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/dispatch"
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
