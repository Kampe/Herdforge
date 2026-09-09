package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
)

func TestProductionDispatcherDisabledGovernorDoesNotInstallTypedNil(t *testing.T) {
	d := dispatch.NewProductionDispatcher(&config.Config{}, nil, nil)
	if err := attachResourceGovernor(d, &config.Config{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if d.Resources != nil {
		t.Fatalf("disabled policy installed a non-nil interface containing %T", d.Resources)
	}
}
