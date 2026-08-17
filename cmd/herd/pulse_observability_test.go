package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

func TestPacketPendingFromExplainFailsClosed(t *testing.T) {
	if packetPendingFromExplain(herdr.AgentExplain{VisibleIdle: true}) {
		t.Fatal("visible idle pane should not be packet-pending")
	}
	if !packetPendingFromExplain(herdr.AgentExplain{}) {
		t.Fatal("unclassified pane must remain packet-pending")
	}
}
