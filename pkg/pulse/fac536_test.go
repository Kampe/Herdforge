package pulse

import (
	"strings"
	"testing"
	"time"
)

func TestPlanBrokerUnavailableBlocksDispatch(t *testing.T) {
	snap, err := Plan(Observation{
		Provider: ProviderObservation{Known: true, Claimable: 1},
		Herdr:    HerdrObservation{Known: true},
		Review:   ReviewObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true},
		Broker:   BrokerObservation{Known: true, Error: "dial unix: no such file"},
	}, Options{Act: true, Spawn: true, Now: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.DispatchBlocked || !strings.Contains(snap.DispatchBlockReason, "broker") {
		t.Fatalf("broker outage must block dispatch: %+v", snap)
	}
	if !snap.UnknownCritical || snap.ExitCode == 0 {
		t.Fatalf("broker outage must be critical and non-zero: %+v", snap)
	}
}
