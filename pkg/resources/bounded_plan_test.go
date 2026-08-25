package resources

import "testing"

type fixedFS struct{}

func (fixedFS) StatFS(string) (Capacity, error) {
	return Capacity{FilesystemID: "fs1", TotalBytes: 1 << 45, FreeBytes: 1 << 40,
		TotalInodes: 1 << 22, FreeInodes: 1 << 21}, nil
}

// FAC-635: a batch of 333 items on one filesystem must be sized for the pipeline's
// peak concurrency, not its queue depth. Summing all of them demanded 62 GiB for
// work whose real peak is 1.5 GiB, and it got worse as the backlog grew.
func TestPlanDiskAdmissionBounded_SizesForPeakNotQueueDepth(t *testing.T) {
	g := &CapacityGate{Backend: fixedFS{}}
	unit := DefaultMergeRequirement()
	reqs := make([]DiskRequest, 0, 333)
	for i := 0; i < 333; i++ {
		reqs = append(reqs, DiskRequest{Operation: "harvest_batch", Path: "/",
			RequiredBytes: unit.Bytes, RequiredInodes: unit.Inodes})
	}

	unbounded, err := g.PlanDiskAdmissionBounded(reqs, 0)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := g.PlanDiskAdmissionBounded(reqs, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded.Requests) != 1 || len(bounded.Requests) != 1 {
		t.Fatalf("one filesystem should collapse to one request: %d / %d",
			len(unbounded.Requests), len(bounded.Requests))
	}
	want := unit.Bytes * 8
	if bounded.Requests[0].RequiredBytes != want {
		t.Fatalf("bounded required %d, want %d (8 x unit)",
			bounded.Requests[0].RequiredBytes, want)
	}
	if unbounded.Requests[0].RequiredBytes <= bounded.Requests[0].RequiredBytes {
		t.Fatal("unbounded must still sum everything; the cap has to be what changed")
	}
}

// maxConcurrent must never INFLATE a small batch: three items with a cap of eight
// require three units, not eight.
func TestPlanDiskAdmissionBounded_DoesNotInflateSmallBatches(t *testing.T) {
	g := &CapacityGate{Backend: fixedFS{}}
	unit := DefaultMergeRequirement()
	reqs := []DiskRequest{
		{Operation: "harvest_batch", Path: "/", RequiredBytes: unit.Bytes, RequiredInodes: unit.Inodes},
		{Operation: "harvest_batch", Path: "/", RequiredBytes: unit.Bytes, RequiredInodes: unit.Inodes},
		{Operation: "harvest_batch", Path: "/", RequiredBytes: unit.Bytes, RequiredInodes: unit.Inodes},
	}
	plan, err := g.PlanDiskAdmissionBounded(reqs, 8)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Requests[0].RequiredBytes != unit.Bytes*3 {
		t.Fatalf("3 items under a cap of 8 must require 3 units, got %d", plan.Requests[0].RequiredBytes)
	}
}
