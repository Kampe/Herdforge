package workbroker

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestDecideBrokerRequiresTaskIdentity(t *testing.T) {
	_, err := ValidateBrokerRecord(BrokerRecord{
		Admission: AdmissionAdmitBuilder,
		Kind:      BrokerKindBuilder,
		Progress:  ProgressUseful,
	})
	if err == nil || !strings.Contains(err.Error(), "missing task identity") {
		t.Fatalf("selector without identity must fail, err=%v", err)
	}
}

func TestDecideBrokerAdmitsReadyBuilderWhileReviewSaturated(t *testing.T) {
	rec, err := DecideBroker(BrokerSnapshot{
		ReviewInFlight: 8,
		ReviewCap:      8,
		Signal:         "work",
		Candidates: []BrokerCandidate{
			{Ref: "FAC-581", ID: "eow6wtnnj7dm7q159dcuwsz6", Priority: 4, Ready: true},
			{Ref: "FAC-9", ID: "rev-1", Priority: 3, Review: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Admission != AdmissionAdmitBuilder || rec.TaskRef != "FAC-581" || rec.TaskID == "" {
		t.Fatalf("ready builder must be admitted with identity, got %+v", rec)
	}
	if !rec.DependencyReady || rec.WaitReason != "" {
		t.Fatalf("builder admission must not wait on review saturation: %+v", rec)
	}
}

func TestDecideBrokerWaitsOnOpenDependency(t *testing.T) {
	rec, err := DecideBroker(BrokerSnapshot{
		Signal: "work",
		Candidates: []BrokerCandidate{
			{Ref: "FAC-75", ID: "t1", Priority: 4, Ready: false, BlockedBy: []string{"FAC-136"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Admission != AdmissionWait || rec.WaitReason != "dependency_blocked" {
		t.Fatalf("blocked builder must wait, got %+v", rec)
	}
	if rec.DependencyReady || rec.TaskRef != "FAC-75" || len(rec.BlockedBy) != 1 || rec.BlockedBy[0] != "FAC-136" {
		t.Fatalf("wait record must name the blocked identity: %+v", rec)
	}
}

func TestDecideBrokerEventWaitOnUnchangedProbe(t *testing.T) {
	for _, tc := range []struct {
		signal string
		last   string
		cur    string
		want   string
	}{
		{signal: "sleep", want: "sleep_is_not_progress"},
		{signal: "probe", want: "identical_probe"},
		{signal: "ack", want: "acknowledgement_only"},
		{signal: "unchanged", want: "unchanged_report"},
		{last: "sha-a", cur: "sha-a", want: "unchanged_report"},
	} {
		rec, err := DecideBroker(BrokerSnapshot{
			Signal:          tc.signal,
			LastArtifact:    tc.last,
			CurrentArtifact: tc.cur,
			Candidates:      []BrokerCandidate{{Ref: "FAC-1", ID: "t1", Priority: 4, Ready: true}},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if rec.Admission != AdmissionWait || rec.WaitReason != tc.want || UsefulProgress(rec.Progress) {
			t.Fatalf("%s: useful work leaked through: %+v", tc.want, rec)
		}
	}
}

func TestDecideBrokerWaitReasonWhenReviewSaturatedAndNoBuilder(t *testing.T) {
	rec, err := DecideBroker(BrokerSnapshot{
		ReviewInFlight: 3,
		ReviewCap:      3,
		Signal:         "work",
		Candidates:     []BrokerCandidate{{Ref: "FAC-9", ID: "r1", Priority: 3, Review: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Admission != AdmissionWait || rec.WaitReason != "review_saturated" || rec.TaskRef != "" {
		t.Fatalf("review-only saturation must wait without a builder identity: %+v", rec)
	}
}

func TestBuilderFromTasksRanksByPriorityThenRef(t *testing.T) {
	rec, err := BuilderFromTasks([]*provider.Task{
		{Ref: "FAC-20", ID: "20", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
		{Ref: "FAC-2", ID: "2", Status: provider.StatusToDo, Priority: provider.PriorityHigh},
		{Ref: "FAC-3", ID: "3", Status: provider.StatusToDo, Priority: provider.PriorityUrgent},
	}, nil, 8, 8, "work")
	if err != nil {
		t.Fatal(err)
	}
	if rec.TaskRef != "FAC-3" || rec.Admission != AdmissionAdmitBuilder {
		t.Fatalf("want FAC-3 despite a full review slot, got %+v", rec)
	}
}
