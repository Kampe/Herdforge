package launch

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

// CallbackWakeTaskPacket is the durable wake/callback contract for task workers.
const CallbackWakeTaskPacket = "wake-task-packet-v1"

// CallbackLaneStanding is the standing-lane callback contract.
const CallbackLaneStanding = "lane-standing-v1"

// BoundarySpec is the complete input to the write-capable launch boundary
// (FAC-139). Admit validates decision + probe proof before any Tab/process
// side effect; Open additionally creates the tab through TabOpener.
type BoundarySpec struct {
	// Decision is the router-issued LaunchDecision (required).
	Decision *router.LaunchDecision
	// Request carries task/lease/repository/lane binding for Validate.
	Request Request
	// Probe is a current artifact-backed tool-probe receipt for the decision's
	// surface identity. Required for write-capable launches.
	Probe *toolprobe.Receipt
	// Lane, when set, enforces Authority / Capabilities / IncompatibleWith.
	Lane *config.LaneDef
	// Workspace, Label, Cwd are the Herdr tab placement (required for Open).
	Workspace string
	Label     string
	Cwd       string
	Env       []string
	NoFocus   bool
	// WriteCapable defaults true when Decision role is worker/reviewer/forge/recovery.
	// Set explicitly false only for read-only observation paths.
	WriteCapable *bool
	// CallbackContract overrides the default role-derived callback name.
	CallbackContract string
	// Sink records rejection receipts (optional).
	Sink Sink
	// Now is injectable clock for probe freshness (optional).
	Now func() time.Time
	// RosterRoles lists every role present in the active roster. Required when
	// Lane.IncompatibleWith is non-empty so self-incompatibility can be checked
	// against a known set (double-launch of incompatible roles is a higher-level
	// concern; here we refuse a lane that lists its own role as incompatible).
	RosterRoles []string
}

// Plan is the exact, admitted launch contract. Model/effort/argv come only
// from LaunchDecision — never harness defaults.
type Plan struct {
	Provider         string
	Model            string
	Effort           string
	Pool             string
	Role             string
	Shape            string
	Family           string
	Harness          string
	Argv             []string
	HarnessArgv      []string
	Cwd              string
	Label            string
	Workspace        string
	Env              []string
	NoFocus          bool
	ProbeKey         string
	DecisionDigest   string
	ProbeSignature   string
	ProbeStatus      toolprobe.Status
	CallbackContract string
	Decision         *router.LaunchDecision
	Probe            *toolprobe.Receipt
}

// TabOpener creates a Herdr tab. Implemented by production herdr adapters and
// test fakes; launch never imports herdr (import cycle).
type TabOpener interface {
	OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (tabID, paneID string, err error)
}

// Admit validates decision + probe + lane policy and returns the launch Plan.
// It never creates a tab or starts an agent.
func Admit(spec BoundarySpec) (*Plan, error) {
	now := time.Now().UTC()
	if spec.Now != nil {
		now = spec.Now().UTC()
	}
	req := spec.Request
	if req.Decision == nil {
		req.Decision = spec.Decision
	}
	if spec.Decision == nil {
		return nil, rejectBoundary(req, spec.Sink, "LaunchDecision is required")
	}
	// Ensure Request.Decision is the admitted decision for Validate/digest.
	req.Decision = spec.Decision

	if err := Validate(req, spec.Sink); err != nil {
		return nil, err
	}

	writeCapable := roleWriteCapable(spec.Decision.Role)
	if spec.WriteCapable != nil {
		writeCapable = *spec.WriteCapable
	}

	if writeCapable {
		if err := requireProbe(spec.Decision, spec.Probe, now); err != nil {
			return nil, rejectBoundary(req, spec.Sink, err.Error())
		}
	}

	if err := enforceLane(spec.Lane, spec.Decision, writeCapable, spec.RosterRoles); err != nil {
		return nil, rejectBoundary(req, spec.Sink, err.Error())
	}

	// Recheck family / decision integrity at the launch edge.
	if err := router.VerifyDecisionForScope(spec.Decision, req.TaskRef, req.LeaseGeneration, req.Scope); err != nil {
		return nil, rejectBoundary(req, spec.Sink, "decision integrity: "+err.Error())
	}

	callback := strings.TrimSpace(spec.CallbackContract)
	if callback == "" {
		callback = defaultCallback(spec.Decision.Role, req.Scope)
	}

	plan := &Plan{
		Provider:         spec.Decision.Provider,
		Model:            spec.Decision.Model,
		Effort:           spec.Decision.Effort,
		Pool:             spec.Decision.Pool,
		Role:             string(spec.Decision.Role),
		Shape:            spec.Decision.Shape,
		Family:           spec.Decision.Family,
		Harness:          spec.Decision.Harness,
		Argv:             clone(spec.Decision.Argv),
		HarnessArgv:      clone(spec.Decision.HarnessArgv),
		Cwd:              strings.TrimSpace(spec.Cwd),
		Label:            strings.TrimSpace(spec.Label),
		Workspace:        strings.TrimSpace(spec.Workspace),
		Env:              clone(spec.Env),
		NoFocus:          spec.NoFocus,
		ProbeKey:         spec.Decision.ProbeKey,
		DecisionDigest:   DecisionDigest(spec.Decision),
		CallbackContract: callback,
		Decision:         spec.Decision,
	}
	if spec.Probe != nil {
		plan.ProbeSignature = spec.Probe.Signature
		plan.ProbeStatus = spec.Probe.Status
		plan.Probe = spec.Probe
	}
	if writeCapable && (plan.Model == "" || plan.Effort == "" || len(plan.Argv) == 0) {
		return nil, rejectBoundary(req, spec.Sink, "write-capable plan missing model, effort, or argv from LaunchDecision")
	}
	return plan, nil
}

// Open admits the launch and creates the tab. Missing/stale probe or decision
// fails before OpenTab is invoked.
func Open(opener TabOpener, spec BoundarySpec) (*Plan, string, string, error) {
	if strings.TrimSpace(spec.Workspace) == "" || strings.TrimSpace(spec.Label) == "" || strings.TrimSpace(spec.Cwd) == "" {
		return nil, "", "", rejectBoundary(spec.Request, spec.Sink, "workspace, label, and cwd are required to open a write-capable tab")
	}
	plan, err := Admit(spec)
	if err != nil {
		return nil, "", "", err
	}
	if opener == nil {
		return nil, "", "", rejectBoundary(spec.Request, spec.Sink, "tab opener is required")
	}
	tabID, paneID, err := opener.OpenTab(plan.Workspace, plan.Label, plan.Cwd, plan.NoFocus, plan.Env...)
	if err != nil {
		return nil, "", "", fmt.Errorf("launch boundary tab open: %w", err)
	}
	return plan, tabID, paneID, nil
}

func requireProbe(d *router.LaunchDecision, probe *toolprobe.Receipt, now time.Time) error {
	if probe == nil {
		return fmt.Errorf("artifact tool-probe PASS receipt is required for write-capable launch")
	}
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		return err
	}
	// Surface identity must match exactly; task/lease on the receipt may be empty
	// (cache reuse) but must not contradict the decision when set.
	if !probe.Identity.Matches(id) {
		return fmt.Errorf("probe identity does not match LaunchDecision surface")
	}
	if probe.Identity.TaskRef != "" && id.TaskRef != "" && probe.Identity.TaskRef != id.TaskRef {
		return fmt.Errorf("probe task_ref %q does not match decision task_ref %q", probe.Identity.TaskRef, id.TaskRef)
	}
	if probe.Identity.LeaseGeneration > 0 && id.LeaseGeneration > 0 && probe.Identity.LeaseGeneration != id.LeaseGeneration {
		return fmt.Errorf("probe lease generation %d does not match decision %d", probe.Identity.LeaseGeneration, id.LeaseGeneration)
	}
	if err := probe.VerifySignature(); err != nil {
		return fmt.Errorf("probe receipt: %w", err)
	}
	if !probe.Passes(now) {
		if !probe.Fresh(now) {
			return fmt.Errorf("tool-probe receipt is stale or schema-mismatched (status=%s)", probe.Status)
		}
		return fmt.Errorf("tool-probe status %s blocks write-capable launch: %s", probe.Status, probe.Reason)
	}
	return nil
}

func enforceLane(lane *config.LaneDef, d *router.LaunchDecision, writeCapable bool, roster []string) error {
	if lane == nil {
		return nil
	}
	if writeCapable && lane.Authority == config.AuthorityRead {
		return fmt.Errorf("lane %q has authority=read; write-capable launch forbidden", lane.Name)
	}
	if writeCapable {
		for _, cap := range lane.Capabilities {
			switch cap {
			case config.CapabilityGitWrite, config.CapabilityFSWrite, config.CapabilityShellExec:
				// Write capabilities require a tool-probe PASS (checked separately).
				// Presence of the capability on a write lane is expected.
			case config.CapabilityBoardWrite, config.CapabilityNetwork:
				// Board/network are not tool-artifact gates.
			default:
				if cap != "" && !validCap(cap) {
					return fmt.Errorf("lane %q declares unknown capability %q", lane.Name, cap)
				}
			}
		}
	}
	role := string(d.Role)
	for _, bad := range lane.IncompatibleWith {
		if strings.EqualFold(strings.TrimSpace(bad), role) {
			return fmt.Errorf("lane %q lists its own role %q in incompatible_with", lane.Name, role)
		}
	}
	// Optional roster presence check: every incompatible role should exist.
	if len(roster) > 0 {
		known := map[string]bool{}
		for _, r := range roster {
			known[strings.ToLower(strings.TrimSpace(r))] = true
		}
		for _, bad := range lane.IncompatibleWith {
			if !known[strings.ToLower(strings.TrimSpace(bad))] {
				return fmt.Errorf("lane %q incompatible_with unknown role %q", lane.Name, bad)
			}
		}
	}
	return nil
}

func validCap(c config.Capability) bool {
	switch c {
	case config.CapabilityNetwork, config.CapabilityGitWrite, config.CapabilityFSWrite,
		config.CapabilityBoardWrite, config.CapabilityShellExec:
		return true
	default:
		return false
	}
}

func roleWriteCapable(role router.Role) bool {
	switch role {
	case router.RoleWorker, router.RoleForgeSmith, router.RoleRecovery,
		router.RoleReviewer, router.RoleAssayer:
		return true
	default:
		return false
	}
}

func defaultCallback(role router.Role, scope string) string {
	if scope == router.ScopeLane {
		return CallbackLaneStanding
	}
	switch role {
	case router.RoleReviewer, router.RoleAssayer:
		return CallbackWakeTaskPacket
	default:
		return CallbackWakeTaskPacket
	}
}

func rejectBoundary(req Request, sink Sink, reason string) error {
	if sink == nil {
		// Still record via default sink when possible; Validate path already does.
		return fmt.Errorf("launch rejected: %s", reason)
	}
	return reject(req, sink, reason)
}

// BoolPtr is a convenience for BoundarySpec.WriteCapable.
func BoolPtr(v bool) *bool { return &v }
