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
	oA, err := d.ControlFactory(context.Background(), a, a.Lane)
	if err != nil {
		t.Fatal(err)
	}
	oB, err := d.ControlFactory(context.Background(), b, b.Lane)
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
