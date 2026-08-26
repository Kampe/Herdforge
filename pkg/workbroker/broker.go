// Package workbroker is the deep work-broker decision seam: dependency
// readiness, exact task selection, builder admission, progress classification,
// and event-wait. A selector must identify a task or emit an explicit wait
// reason. Review saturation never vetoes an independent ready builder.
package workbroker

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// AdmissionDecision is the work-broker's builder/review/wait outcome.
type AdmissionDecision string

const (
	AdmissionAdmitBuilder AdmissionDecision = "admit_builder"
	AdmissionAdmitReview  AdmissionDecision = "admit_review"
	AdmissionWait         AdmissionDecision = "wait"
	AdmissionRefuse       AdmissionDecision = "refuse"
)

// ProgressClass is how the broker scores one observation.
type ProgressClass string

const (
	ProgressUseful    ProgressClass = "useful"
	ProgressUnchanged ProgressClass = "unchanged"
	ProgressProbe     ProgressClass = "probe"
	ProgressSleep     ProgressClass = "sleep"
	ProgressAckOnly   ProgressClass = "acknowledgement"
	ProgressEventWait ProgressClass = "event_wait"
)

// BrokerKind is the consumer the decision is for. Review admission stays a
// separate adapter: saturation never vetoes an independent builder.
type BrokerKind string

const (
	BrokerKindBuilder BrokerKind = "builder"
	BrokerKindReview  BrokerKind = "review"
	BrokerKindWait    BrokerKind = "wait"
)

// BrokerRecord is the hermetic decision the rest of the control plane consumes.
// A selector must identify a task or emit an explicit wait reason.
type BrokerRecord struct {
	TaskRef         string
	TaskID          string
	DependencyReady bool
	BlockedBy       []string
	Admission       AdmissionDecision
	Kind            BrokerKind
	Progress        ProgressClass
	LastArtifact    string
	WaitReason      string
}

// BrokerCandidate is one ranked board row the broker may select.
type BrokerCandidate struct {
	Ref       string
	ID        string
	Priority  int
	Ready     bool
	BlockedBy []string
	Review    bool
}

// BrokerSnapshot is one observation the broker scores. It is hermetic: no
// provider I/O, no sleep, no acknowledgement side effects.
type BrokerSnapshot struct {
	Candidates      []BrokerCandidate
	ReviewInFlight  int
	ReviewCap       int
	LastArtifact    string
	CurrentArtifact string
	Signal          string
}

// ClassifyProgress scores a probe. Identical artifacts, sleep, acknowledgement,
// and unchanged reports are not useful work.
func ClassifyProgress(signal, lastArtifact, currentArtifact string) ProgressClass {
	switch strings.ToLower(strings.TrimSpace(signal)) {
	case "sleep":
		return ProgressSleep
	case "probe":
		return ProgressProbe
	case "ack", "acknowledgement":
		return ProgressAckOnly
	case "unchanged":
		return ProgressUnchanged
	case "wait":
		return ProgressEventWait
	}
	last := strings.TrimSpace(lastArtifact)
	cur := strings.TrimSpace(currentArtifact)
	if last != "" && cur != "" && last == cur {
		return ProgressUnchanged
	}
	if strings.EqualFold(strings.TrimSpace(signal), "work") {
		return ProgressUseful
	}
	if cur != "" && cur != last {
		return ProgressUseful
	}
	if last == "" && cur == "" && strings.TrimSpace(signal) == "" {
		return ProgressUseful
	}
	return ProgressUnchanged
}

// UsefulProgress reports whether the class may advance a continuation budget.
func UsefulProgress(p ProgressClass) bool {
	return p == ProgressUseful
}

func waitReasonForProgress(p ProgressClass) string {
	switch p {
	case ProgressSleep:
		return "sleep_is_not_progress"
	case ProgressProbe:
		return "identical_probe"
	case ProgressAckOnly:
		return "acknowledgement_only"
	case ProgressUnchanged:
		return "unchanged_report"
	default:
		return "event_wait"
	}
}

func betterCandidate(best, cand *BrokerCandidate) bool {
	if best == nil {
		return true
	}
	if cand.Priority != best.Priority {
		return cand.Priority > best.Priority
	}
	return provider.CompareRefs(cand.Ref, best.Ref) < 0
}

// DecideBroker is the deep work-broker seam: dependency readiness, exact task
// selection, builder admission, progress classification, and event-wait.
func DecideBroker(snap BrokerSnapshot) (BrokerRecord, error) {
	progress := ClassifyProgress(snap.Signal, snap.LastArtifact, snap.CurrentArtifact)
	rec := BrokerRecord{
		Progress:     progress,
		LastArtifact: strings.TrimSpace(snap.CurrentArtifact),
	}
	if rec.LastArtifact == "" {
		rec.LastArtifact = strings.TrimSpace(snap.LastArtifact)
	}
	if !UsefulProgress(progress) {
		rec.Admission = AdmissionWait
		rec.Kind = BrokerKindWait
		rec.WaitReason = waitReasonForProgress(progress)
		return ValidateBrokerRecord(rec)
	}

	reviewSat := snap.ReviewCap > 0 && snap.ReviewInFlight >= snap.ReviewCap
	var bestBuilder, bestBlocked, bestReview *BrokerCandidate
	for i := range snap.Candidates {
		c := &snap.Candidates[i]
		if strings.TrimSpace(c.Ref) == "" && strings.TrimSpace(c.ID) == "" {
			continue
		}
		if c.Review {
			if betterCandidate(bestReview, c) {
				bestReview = c
			}
			continue
		}
		if c.Ready {
			if betterCandidate(bestBuilder, c) {
				bestBuilder = c
			}
			continue
		}
		if betterCandidate(bestBlocked, c) {
			bestBlocked = c
		}
	}

	if bestBuilder != nil {
		rec.TaskRef = bestBuilder.Ref
		rec.TaskID = bestBuilder.ID
		rec.DependencyReady = true
		rec.Admission = AdmissionAdmitBuilder
		rec.Kind = BrokerKindBuilder
		return ValidateBrokerRecord(rec)
	}
	if bestBlocked != nil {
		rec.TaskRef = bestBlocked.Ref
		rec.TaskID = bestBlocked.ID
		rec.DependencyReady = false
		rec.BlockedBy = append([]string(nil), bestBlocked.BlockedBy...)
		rec.Admission = AdmissionWait
		rec.Kind = BrokerKindWait
		rec.WaitReason = "dependency_blocked"
		return ValidateBrokerRecord(rec)
	}
	if bestReview != nil && !reviewSat {
		rec.TaskRef = bestReview.Ref
		rec.TaskID = bestReview.ID
		rec.DependencyReady = true
		rec.Admission = AdmissionAdmitReview
		rec.Kind = BrokerKindReview
		return ValidateBrokerRecord(rec)
	}

	rec.Admission = AdmissionWait
	rec.Kind = BrokerKindWait
	if reviewSat {
		rec.WaitReason = "review_saturated"
	} else {
		rec.WaitReason = "no_claimable_work"
	}
	return ValidateBrokerRecord(rec)
}

// ValidateBrokerRecord is the negative identity gate: admit_builder and
// admit_review require a task identity; wait/refuse require an explicit reason.
func ValidateBrokerRecord(rec BrokerRecord) (BrokerRecord, error) {
	hasID := strings.TrimSpace(rec.TaskRef) != "" || strings.TrimSpace(rec.TaskID) != ""
	switch rec.Admission {
	case AdmissionAdmitBuilder, AdmissionAdmitReview:
		if !hasID {
			return BrokerRecord{}, fmt.Errorf("workbroker: selector output missing task identity")
		}
	case AdmissionWait, AdmissionRefuse:
		if strings.TrimSpace(rec.WaitReason) == "" {
			return BrokerRecord{}, fmt.Errorf("workbroker: wait missing explicit reason")
		}
	default:
		return BrokerRecord{}, fmt.Errorf("workbroker: missing admission decision")
	}
	if rec.Progress == "" {
		return BrokerRecord{}, fmt.Errorf("workbroker: missing progress classification")
	}
	return rec, nil
}

// BuilderFromTasks selects the exact claimable builder or returns a wait record.
func BuilderFromTasks(tasks []*provider.Task, ready func(*provider.Task) (bool, []string), reviewInFlight, reviewCap int, signal string) (BrokerRecord, error) {
	cands := make([]BrokerCandidate, 0, len(tasks))
	for _, task := range tasks {
		if task == nil {
			continue
		}
		isReady, blockedBy := true, []string(nil)
		if ready != nil {
			isReady, blockedBy = ready(task)
		}
		cands = append(cands, BrokerCandidate{
			Ref:       strings.TrimSpace(task.Ref),
			ID:        strings.TrimSpace(task.ID),
			Priority:  PriorityRank(task.Priority),
			Ready:     isReady,
			BlockedBy: blockedBy,
		})
	}
	return DecideBroker(BrokerSnapshot{
		Candidates:     cands,
		ReviewInFlight: reviewInFlight,
		ReviewCap:      reviewCap,
		Signal:         signal,
	})
}

// PriorityRank is the shared selector rank: urgent > high > medium > low.
func PriorityRank(p provider.Priority) int {
	switch p {
	case provider.PriorityUrgent:
		return 4
	case provider.PriorityHigh:
		return 3
	case provider.PriorityMedium:
		return 2
	case provider.PriorityLow:
		return 1
	default:
		return 0
	}
}
