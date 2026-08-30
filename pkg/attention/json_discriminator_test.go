package attention

import (
	"encoding/json"
	"strings"
	"testing"
)

// FAC-634: FAC-604 fixed the human summary and left the JSON surface emitting
// {total:0, needing:0} with exit 0, so a machine consumer still read a false
// green from the exact state that fix existed to flag.
func TestResultJSON_ZeroLaneScanIsUnknownNotHealthy(t *testing.T) {
	raw, err := json.Marshal(Result{Total: 0, Needing: 0})
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"scanned":false`) {
		t.Errorf("must carry a scanned discriminator: %s", body)
	}
	if !strings.Contains(body, `"state":"UNKNOWN"`) {
		t.Errorf("a zero-lane scan must report UNKNOWN: %s", body)
	}
	if strings.Contains(body, "HEALTHY") {
		t.Errorf("nothing scanned must never report HEALTHY: %s", body)
	}
}

// A real scan that finds nothing wrong must still be reportable as healthy, so
// the guard cannot be satisfied by never emitting HEALTHY.
func TestResultJSON_RealCleanScanIsHealthy(t *testing.T) {
	raw, _ := json.Marshal(Result{Total: 14, Needing: 0})
	body := string(raw)
	if !strings.Contains(body, `"state":"HEALTHY"`) || !strings.Contains(body, `"scanned":true`) {
		t.Fatalf("a real clean scan must be HEALTHY and scanned: %s", body)
	}
}

func TestResultJSON_LanesNeedingEyesIsAttention(t *testing.T) {
	raw, _ := json.Marshal(Result{Total: 14, Needing: 3})
	if !strings.Contains(string(raw), `"state":"ATTENTION"`) {
		t.Fatalf("lanes needing eyes must report ATTENTION: %s", raw)
	}
}

func TestResultJSON_ReadyCandidateIsCriticalAttention(t *testing.T) {
	raw, err := json.Marshal(Result{Total: 14, ReadyCandidates: 1})
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"state":"ATTENTION"`) || !strings.Contains(body, `"summary":"herd-attention: CRITICAL`) {
		t.Fatalf("a ready candidate must make the shipped JSON surface critical attention: %s", body)
	}
}
