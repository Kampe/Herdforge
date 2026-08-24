package resources

import (
	"strings"
	"testing"
)

// A harvest request that names no byte/inode requirement must not be reported as
// disk pressure. The gate still refuses -- it is fail-closed -- but announcing it
// as a capacity block with free_bytes:0 made a lane go check df, which showed
// 114 GiB free, while the real fault (an empty DiskRequest) went unread.
func TestPlanDiskAdmission_MalformedRequestIsNotDiskPressure(t *testing.T) {
	g := &CapacityGate{Backend: stubBackend{}}
	_, err := g.PlanDiskAdmission([]DiskRequest{{Operation: "harvest_batch", Path: "/", RequiredBytes: 0, RequiredInodes: 0}})
	if err == nil {
		t.Fatal("a request with no requirement must still be refused")
	}
	msg := err.Error()
	if strings.Contains(msg, "disk capacity gate blocked") {
		t.Fatalf("malformed request must not be announced as a capacity block: %s", msg)
	}
	for _, want := range []string{"malformed", "disk was never the problem"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message must name the real fault (%q missing): %s", want, msg)
		}
	}
	var de *DiskAdmissionError
	if !asDiskErr(err, &de) {
		t.Fatalf("wrong error type: %T", err)
	}
	if de.Decision.Evidence.Reason != DiskReasonInvalidRequest {
		t.Fatalf("reason = %q, want %q", de.Decision.Evidence.Reason, DiskReasonInvalidRequest)
	}
	// Retrying a probe cannot fix a caller that supplied nothing to check.
	if de.Decision.Evidence.NextAction == DiskActionRetryProbe {
		t.Fatal("next action must not be retry_capacity_probe for a malformed request")
	}
}

func asDiskErr(err error, out **DiskAdmissionError) bool {
	if v, ok := err.(*DiskAdmissionError); ok {
		*out = v
		return true
	}
	return false
}

type stubBackend struct{}

func (stubBackend) StatFS(string) (Capacity, error) {
	return Capacity{FilesystemID: "fs", TotalBytes: 1 << 40, FreeBytes: 1 << 39, TotalInodes: 1 << 20, FreeInodes: 1 << 19}, nil
}
