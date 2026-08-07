package signerboundary

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ErrVerdictNotAdmitted is returned when a structurally valid SignRequest is
// not admitted by the reviewer path (FAC-145 integration hook).
var ErrVerdictNotAdmitted = fmt.Errorf("signerboundary: verdict not admitted by reviewer path")

// AdmissionFunc is the narrow FAC-145 hook: after syntactic ValidateProduction
// and peer/MAC checks, the server refuses to sign unadmitted reviewer verdicts.
// Production FAC-145 installs a ledger/task-context check; the default admits
// only payloads that bind exact candidate/base/verdict fields.
type AdmissionFunc func(req SignRequest) error

var (
	admissionMu sync.RWMutex
	globalAdmit AdmissionFunc = DefaultAdmitReviewerVerdict
)

// SetAdmission installs the process-wide admission hook (FAC-145 consumer).
// nil restores DefaultAdmitReviewerVerdict.
func SetAdmission(fn AdmissionFunc) {
	admissionMu.Lock()
	defer admissionMu.Unlock()
	if fn == nil {
		globalAdmit = DefaultAdmitReviewerVerdict
		return
	}
	globalAdmit = fn
}

func currentAdmission() AdmissionFunc {
	admissionMu.RLock()
	defer admissionMu.RUnlock()
	return globalAdmit
}

// DefaultAdmitReviewerVerdict refuses sign-verdict when the canonical payload
// does not bind the same candidate_sha, base_sha, and verdict as the request
// fields. This blocks "structurally valid but unadmitted" free-form JSON.
// FAC-145 replaces/extends this with durable task-context admission.
func DefaultAdmitReviewerVerdict(req SignRequest) error {
	switch req.Op {
	case OpPing, OpProbe:
		return nil
	case OpSignReceipt:
		// Receipts require non-empty payload (ValidateProduction); deeper ledger
		// binding is FAC-145. Still require embedded candidate_sha match when present.
		return admitPayloadFieldMatch(req, false)
	case OpSignVerdict:
		return admitPayloadFieldMatch(req, true)
	default:
		return fmt.Errorf("%w: op %q", ErrVerdictNotAdmitted, req.Op)
	}
}

func admitPayloadFieldMatch(req SignRequest, requireVerdict bool) error {
	p := req.payloadBytes()
	if len(p) == 0 {
		return fmt.Errorf("%w: empty payload", ErrVerdictNotAdmitted)
	}
	var body map[string]any
	if err := json.Unmarshal(p, &body); err != nil {
		return fmt.Errorf("%w: payload must be JSON object binding review fields: %v", ErrVerdictNotAdmitted, err)
	}
	if err := matchJSONString(body, []string{"candidate_sha", "candidateSha", "sha"}, req.CandidateSHA); err != nil {
		return err
	}
	if err := matchJSONString(body, []string{"base_sha", "baseSha", "base"}, req.BaseSHA); err != nil {
		return err
	}
	if requireVerdict {
		if err := matchJSONString(body, []string{"verdict", "decision", "status"}, req.Verdict); err != nil {
			return err
		}
	}
	// Optional patch_id when present in JSON must match.
	if v, ok := firstString(body, []string{"patch_id", "patchId", "patch"}); ok && strings.TrimSpace(req.PatchID) != "" {
		if strings.TrimSpace(v) != strings.TrimSpace(req.PatchID) {
			return fmt.Errorf("%w: payload patch_id %q != request %q", ErrVerdictNotAdmitted, v, req.PatchID)
		}
	}
	return nil
}

func matchJSONString(body map[string]any, keys []string, want string) error {
	got, ok := firstString(body, keys)
	if !ok {
		return fmt.Errorf("%w: payload missing binding field among %v", ErrVerdictNotAdmitted, keys)
	}
	if !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want)) {
		return fmt.Errorf("%w: payload field %q != request %q", ErrVerdictNotAdmitted, got, want)
	}
	return nil
}

func firstString(body map[string]any, keys []string) (string, bool) {
	for _, k := range keys {
		if v, ok := body[k]; ok {
			switch t := v.(type) {
			case string:
				if strings.TrimSpace(t) != "" {
					return t, true
				}
			}
		}
	}
	return "", false
}

// AdmitRequest runs ValidateProduction then the admission hook.
func AdmitRequest(req SignRequest) error {
	if err := req.ValidateProduction(); err != nil {
		return err
	}
	fn := currentAdmission()
	if fn == nil {
		return nil
	}
	return fn(req)
}
