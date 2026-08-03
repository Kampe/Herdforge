// Package eligibility is the board eligibility gate (FAC-123).
//
// To Do is a board column, not automatic claimability. Only cards that pass
// acceptance, role, dependency, duplicate, and already-integrated checks may
// enter deterministic selection. This package consumes TaskProvider read-only
// and accepts external relation/evidence facts until FAC-124 enriches adapters.
package eligibility

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// State is the eligibility classification of one card.
type State string

const (
	// StateEligible means the card may enter claim selection for a matching role.
	StateEligible State = "ELIGIBLE"
	// StateNeedsGrooming means required fields/authority are missing.
	StateNeedsGrooming State = "NEEDS_GROOMING"
	// StateBlocked means an open dependency, duplicate, or claim-path role mismatch holds the card back.
	StateBlocked State = "BLOCKED"
	// StateAlreadyDone means the card is done or already integrated.
	StateAlreadyDone State = "ALREADY_DONE"
)

// Result is the provider-neutral eligibility evaluation for one task.
type Result struct {
	Ref                  string            `json:"ref"`
	Title                string            `json:"title"`
	State                State             `json:"state"`
	Priority             provider.Priority `json:"priority"`
	Status               string            `json:"status"` // normalized
	Role                 string            `json:"role,omitempty"`
	RiskHint             string            `json:"risk_hint,omitempty"`
	Reasons              []string          `json:"reasons,omitempty"` // exact missing field/authority
	BlockedBy            []string          `json:"blocked_by,omitempty"`
	DuplicateOf          []string          `json:"duplicate_of,omitempty"`
	IntegratedEvidence   string            `json:"integrated_evidence,omitempty"`
	AcceptancePresent    bool              `json:"acceptance_present"`
	HasRoleLabel         bool              `json:"has_role_label"`
	OperatorPrioritySet  bool              `json:"operator_priority_set"`
}

// Facts carries external evidence the provider Task surface does not yet expose
// (relations, duplicates, integration). Callers supply one provider revision.
type Facts struct {
	// Blockers maps a dependent ref to the refs that block it (inbound blocks).
	// Example: FAC-124 blocks FAC-119 → Blockers["FAC-119"] = ["FAC-124"].
	Blockers map[string][]string
	// OpenRefs is the set of refs not yet done. A blocker still in OpenRefs
	// holds its dependent in StateBlocked.
	OpenRefs map[string]bool
	// Duplicates maps ref → other refs this card duplicates (or is duplicated by).
	Duplicates map[string][]string
	// Integrated maps ref → evidence string that the work is already on main.
	Integrated map[string]string
	// RoleMap is an explicit operator mapping ref → required role. When set for
	// a ref it is authoritative alongside (or instead of) board labels.
	RoleMap map[string]string
	// RiskHints maps ref → risk tier (R0–R3). Labels/description may also supply risk.
	RiskHints map[string]string
}

// Report partitions a board snapshot into eligibility buckets.
// Eligible is sorted by (priority DESC, ticket number ASC).
type Report struct {
	Eligible      []Result `json:"eligible"`
	NeedsGrooming []Result `json:"needs_grooming"`
	Blocked       []Result `json:"blocked"`
	AlreadyDone   []Result `json:"already_done"`
}

// priorityRank is shared with daemon/scout claim-order invariants.
var priorityRank = map[provider.Priority]int{
	provider.PriorityUrgent: 4,
	provider.PriorityHigh:   3,
	provider.PriorityMedium: 2,
	provider.PriorityLow:    1,
}

// NormalizeStatus maps common provider spellings to canonical lifecycle values.
// Unknown non-empty statuses are returned lowercased and prefixed so callers
// never treat them as to-do or done by accident.
func NormalizeStatus(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	switch s {
	case "to-do", "todo", "open", "backlog", "planned", "ready", "new", "triage":
		return "to-do"
	case "in-progress", "inprogress", "doing", "active", "started", "wip":
		return "in-progress"
	case "in-review", "review", "code-review", "pr-review":
		return "in-review"
	case "done", "closed", "complete", "completed", "merged", "resolved", "archived":
		return "done"
	case "":
		return "unknown"
	default:
		if strings.HasPrefix(s, "unknown:") {
			return s
		}
		return "unknown:" + s
	}
}

// HasAcceptanceCriteria reports whether description carries observable acceptance.
// Empty descriptions never pass. Markers: "acceptance criteria"/"criterion" or
// markdown task checkboxes.
func HasAcceptanceCriteria(description string) bool {
	if strings.TrimSpace(description) == "" {
		return false
	}
	lower := strings.ToLower(description)
	if strings.Contains(lower, "acceptance criteria") ||
		strings.Contains(lower, "acceptance criterion") {
		return true
	}
	// Markdown task list items used as acceptance checkboxes on the board.
	for _, line := range strings.Split(description, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "- [ ]") || strings.HasPrefix(trim, "- [x]") ||
			strings.HasPrefix(trim, "- [X]") || strings.HasPrefix(trim, "* [ ]") ||
			strings.HasPrefix(trim, "* [x]") || strings.HasPrefix(trim, "* [X]") {
			return true
		}
	}
	return false
}

// ResolveRole returns the required role for a card from RoleMap (authoritative
// when present) or an exact (case-insensitive) label match among known coding
// roles. Empty means unlabeled — never matches worker/smith by fallback.
func ResolveRole(task *provider.Task, roleMap map[string]string) (role string, ok bool) {
	if task == nil {
		return "", false
	}
	if roleMap != nil {
		if r, found := roleMap[task.Ref]; found && strings.TrimSpace(r) != "" {
			return strings.TrimSpace(r), true
		}
	}
	for _, label := range task.Labels {
		l := strings.TrimSpace(label)
		if l == "" {
			continue
		}
		// Exact role labels used by herd.yaml / board (forge-smith, worker, …).
		// Also accept herd-smith as historical alias of forge-smith/smith.
		switch strings.ToLower(l) {
		case "worker", "forge-smith", "herd-smith", "smith", "reviewer",
			"scout-planner", "orchestrator", "verification-gate", "review-supervisor",
			"harvest", "recovery-sentinel":
			return l, true
		}
		// Labels may be written as role:worker.
		if strings.HasPrefix(strings.ToLower(l), "role:") {
			r := strings.TrimSpace(l[len("role:"):])
			if r != "" {
				return r, true
			}
		}
	}
	return "", false
}

// ResolveRisk returns a risk hint from Facts, labels (risk:R2 / R2), or description.
func ResolveRisk(task *provider.Task, hints map[string]string) string {
	if task == nil {
		return ""
	}
	if hints != nil {
		if r, ok := hints[task.Ref]; ok && strings.TrimSpace(r) != "" {
			return strings.TrimSpace(r)
		}
	}
	for _, label := range task.Labels {
		l := strings.TrimSpace(label)
		lower := strings.ToLower(l)
		if strings.HasPrefix(lower, "risk:") {
			return strings.TrimSpace(l[len("risk:"):])
		}
		switch lower {
		case "r0", "r1", "r2", "r3":
			return strings.ToUpper(lower)
		}
	}
	lower := strings.ToLower(task.Description)
	for _, key := range []string{"risk:", "risk tier:", "risk hint:"} {
		if i := strings.Index(lower, key); i >= 0 {
			rest := strings.TrimSpace(task.Description[i+len(key):])
			// first token
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return strings.Trim(fields[0], ".,;")
			}
		}
	}
	return ""
}

// rolesMatch reports whether claimRole may claim a card with requiredRole.
// Exact match after normalization; herd-smith ≡ forge-smith ≡ smith.
func rolesMatch(required, claim string) bool {
	return normalizeRole(required) == normalizeRole(claim) && normalizeRole(claim) != ""
}

func normalizeRole(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	switch r {
	case "herd-smith", "smith", "forge-smith":
		return "forge-smith"
	default:
		return r
	}
}

// EvaluateTask classifies one task against Facts and an optional claim role.
// claimRole empty disables the role-match gate (hygiene / all-role scan).
// claimRole set requires an exact role label or RoleMap entry that matches.
func EvaluateTask(task *provider.Task, facts Facts, claimRole string) Result {
	if task == nil {
		return Result{
			State:   StateNeedsGrooming,
			Status:  "unknown",
			Reasons: []string{"task: nil task pointer"},
		}
	}

	res := Result{
		Ref:                 task.Ref,
		Title:               task.Title,
		Priority:            task.Priority,
		Status:              NormalizeStatus(task.Status),
		OperatorPrioritySet: strings.TrimSpace(string(task.Priority)) != "",
		AcceptancePresent:   HasAcceptanceCriteria(task.Description),
	}

	role, hasRole := ResolveRole(task, facts.RoleMap)
	res.Role = role
	res.HasRoleLabel = hasRole
	res.RiskHint = ResolveRisk(task, facts.RiskHints)

	if facts.Integrated != nil {
		if ev, ok := facts.Integrated[task.Ref]; ok && strings.TrimSpace(ev) != "" {
			res.IntegratedEvidence = strings.TrimSpace(ev)
		}
	}
	if facts.Duplicates != nil {
		if dups := facts.Duplicates[task.Ref]; len(dups) > 0 {
			res.DuplicateOf = append([]string(nil), dups...)
			sort.Strings(res.DuplicateOf)
		}
	}

	// Open inbound blockers at this provider revision.
	if facts.Blockers != nil {
		for _, b := range facts.Blockers[task.Ref] {
			if facts.OpenRefs != nil && facts.OpenRefs[b] {
				res.BlockedBy = append(res.BlockedBy, b)
			}
		}
		sort.Strings(res.BlockedBy)
	}

	// Terminal: already done / integrated.
	if res.Status == "done" || res.IntegratedEvidence != "" {
		res.State = StateAlreadyDone
		if res.Status == "done" {
			res.Reasons = append(res.Reasons, "status: already done")
		}
		if res.IntegratedEvidence != "" {
			res.Reasons = append(res.Reasons, "integrated: "+res.IntegratedEvidence)
		}
		return res
	}

	// Duplicate evidence blocks claim (unless already classified as done above).
	if len(res.DuplicateOf) > 0 {
		res.State = StateBlocked
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("duplicate: unresolved duplicate of %s", strings.Join(res.DuplicateOf, ", ")))
		return res
	}

	// Dependency-blocked.
	if len(res.BlockedBy) > 0 {
		res.State = StateBlocked
		res.Reasons = append(res.Reasons,
			fmt.Sprintf("dependency: blocked by open %s", strings.Join(res.BlockedBy, ", ")))
		return res
	}

	// Grooming gates — each reason names the exact field/authority missing.
	var grooming []string
	if strings.TrimSpace(task.Description) == "" {
		grooming = append(grooming, "description: empty (operator description required)")
	}
	if !res.AcceptancePresent {
		grooming = append(grooming, "acceptance: missing acceptance criteria (section or checkboxes)")
	}
	if !res.HasRoleLabel {
		grooming = append(grooming, "role: label or explicit role_map entry required (unlabeled matches no coding lane)")
	}
	if !res.OperatorPrioritySet {
		grooming = append(grooming, "priority: operator priority required")
	}
	if strings.TrimSpace(res.RiskHint) == "" {
		grooming = append(grooming, "risk: risk hint required (label risk:R*, description, or facts.RiskHints)")
	}
	if res.Status == "unknown" || strings.HasPrefix(res.Status, "unknown:") {
		grooming = append(grooming, "status: unknown or unnormalized provider status ("+task.Status+")")
	}
	if res.Status != "to-do" && !strings.HasPrefix(res.Status, "unknown") {
		// in-progress / in-review are not claim candidates.
		grooming = append(grooming, "status: not claimable in status "+res.Status+" (need to-do)")
	}

	if len(grooming) > 0 {
		res.State = StateNeedsGrooming
		res.Reasons = grooming
		return res
	}

	// Role match gate when a claim role is specified.
	if strings.TrimSpace(claimRole) != "" {
		if !rolesMatch(res.Role, claimRole) {
			res.State = StateBlocked
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("role_mismatch: card requires %q, claim role is %q", res.Role, claimRole))
			return res
		}
	}

	res.State = StateEligible
	return res
}

// EvaluateBoard lists tasks from the provider (read-only) and classifies them.
// A provider error is returned as-is and is never converted into an empty report.
// statusFilter is passed to ListTasks; empty means provider-defined "all".
func EvaluateBoard(ctx context.Context, tp provider.TaskProvider, projectID, statusFilter string, facts Facts, claimRole string) (*Report, error) {
	if tp == nil {
		return nil, fmt.Errorf("eligibility: task provider is nil")
	}
	tasks, err := tp.ListTasks(ctx, projectID, statusFilter)
	if err != nil {
		// Fail-closed: unknown/error posture must not look like "no candidates".
		return nil, fmt.Errorf("eligibility: list tasks: %w", err)
	}

	rep := &Report{}
	for _, task := range tasks {
		r := EvaluateTask(task, facts, claimRole)
		switch r.State {
		case StateEligible:
			rep.Eligible = append(rep.Eligible, r)
		case StateNeedsGrooming:
			rep.NeedsGrooming = append(rep.NeedsGrooming, r)
		case StateBlocked:
			rep.Blocked = append(rep.Blocked, r)
		case StateAlreadyDone:
			rep.AlreadyDone = append(rep.AlreadyDone, r)
		default:
			// Defensive: treat unexpected state as grooming with explicit reason.
			r.State = StateNeedsGrooming
			r.Reasons = append(r.Reasons, "state: unexpected classification")
			rep.NeedsGrooming = append(rep.NeedsGrooming, r)
		}
	}

	SortByClaimOrder(rep.Eligible)
	SortByClaimOrder(rep.NeedsGrooming)
	SortByClaimOrder(rep.Blocked)
	SortByClaimOrder(rep.AlreadyDone)
	return rep, nil
}

// SelectEligible returns only ELIGIBLE cards for claimRole, sorted by
// (priority DESC, ticket number ASC). Provider errors propagate.
func SelectEligible(ctx context.Context, tp provider.TaskProvider, projectID string, facts Facts, claimRole string) ([]Result, error) {
	if strings.TrimSpace(claimRole) == "" {
		return nil, fmt.Errorf("eligibility: claim role is required for SelectEligible")
	}
	rep, err := EvaluateBoard(ctx, tp, projectID, "to-do", facts, claimRole)
	if err != nil {
		return nil, err
	}
	return rep.Eligible, nil
}

// SortByClaimOrder sorts by priority DESC then numeric ticket ref ASC.
func SortByClaimOrder(rows []Result) {
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := priorityRank[rows[i].Priority], priorityRank[rows[j].Priority]
		if pi != pj {
			return pi > pj
		}
		return provider.CompareRefs(rows[i].Ref, rows[j].Ref) < 0
	})
}
