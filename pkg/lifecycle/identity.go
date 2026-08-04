package lifecycle

import (
	"fmt"
	"strings"
)

type CanonicalLane struct {
	Name string
	Role string
}
type CanonicalLaneRegistry struct{ lanes []CanonicalLane }

func NewCanonicalLaneRegistry(lanes []CanonicalLane) (CanonicalLaneRegistry, error) {
	seenName, seenRole := map[string]bool{}, map[string]bool{}
	copyLanes := make([]CanonicalLane, 0, len(lanes))
	for _, lane := range lanes {
		name, role := strings.TrimSpace(lane.Name), strings.TrimSpace(lane.Role)
		if name == "" || role == "" || seenName[strings.ToLower(name)] || seenRole[strings.ToLower(role)] {
			return CanonicalLaneRegistry{}, fmt.Errorf("invalid or duplicate canonical lane %q", name)
		}
		seenName[strings.ToLower(name)] = true
		seenRole[strings.ToLower(role)] = true
		copyLanes = append(copyLanes, CanonicalLane{Name: name, Role: role})
	}
	return CanonicalLaneRegistry{lanes: copyLanes}, nil
}

func (r CanonicalLaneRegistry) resolve(value, namespace string, match func(CanonicalLane) bool) (CanonicalLane, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return CanonicalLane{}, fmt.Errorf("%s identity is required", namespace)
	}
	for _, lane := range r.lanes {
		if match(lane) {
			return lane, nil
		}
	}
	return CanonicalLane{}, fmt.Errorf("unknown %s %q", namespace, value)
}

func (r CanonicalLaneRegistry) ResolveLaneName(name string) (CanonicalLane, error) {
	return r.resolve(name, "lane", func(l CanonicalLane) bool { return strings.EqualFold(l.Name, strings.TrimSpace(name)) })
}
func (r CanonicalLaneRegistry) ResolveRole(role string) (CanonicalLane, error) {
	return r.resolve(role, "role", func(l CanonicalLane) bool { return strings.EqualFold(l.Role, strings.TrimSpace(role)) })
}
func (r CanonicalLaneRegistry) ResolveLiveAgentID(id string) (CanonicalLane, error) {
	id = strings.TrimSpace(id)
	if len(id) < 6 || !strings.EqualFold(id[:6], "forge-") {
		return CanonicalLane{}, fmt.Errorf("invalid live agent ID %q", id)
	}
	return r.ResolveLaneName(id[6:])
}
func (r CanonicalLaneRegistry) Resolve(value string) (CanonicalLane, error) {
	byName, nameErr := r.ResolveLaneName(value)
	byRole, roleErr := r.ResolveRole(value)
	if nameErr == nil && roleErr == nil && (byName.Name != byRole.Name || byName.Role != byRole.Role) {
		return CanonicalLane{}, fmt.Errorf("ambiguous mixed lane/role namespace %q", value)
	}
	if nameErr == nil {
		return byName, nil
	}
	if roleErr == nil {
		return byRole, nil
	}
	return CanonicalLane{}, fmt.Errorf("unknown lane/role %q", value)
}

// LaneNames returns the validated configured lane-name snapshot in config
// order. Live IDs remain a distinct namespace and use ResolveLiveAgentID.
func (r CanonicalLaneRegistry) LaneNames() []string {
	names := make([]string, 0, len(r.lanes))
	for _, lane := range r.lanes {
		names = append(names, lane.Name)
	}
	return names
}
func (r CanonicalLaneRegistry) Identity(repository, value, task, scope string) (HoldIdentity, error) {
	lane, err := r.ResolveLaneName(value)
	if err != nil {
		return HoldIdentity{}, err
	}
	identity := HoldIdentity{Repository: strings.TrimSpace(repository), Owner: lane.Role, Lane: lane.Name, Task: strings.TrimSpace(task), Scope: scope}
	if !identity.valid() {
		return HoldIdentity{}, fmt.Errorf("invalid canonical identity for %q", value)
	}
	return identity, nil
}
