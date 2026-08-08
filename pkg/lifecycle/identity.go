package lifecycle

import (
	"errors"
	"fmt"
	"strings"
)

// ErrAmbiguousRole is returned by ResolveRole (and propagated by Resolve) when
// multiple lanes carry the same role. The caller must name a lane explicitly.
var ErrAmbiguousRole = errors.New("ambiguous role")

type CanonicalLane struct {
	Name     string
	Role     string
	Standing bool
}
type CanonicalLaneRegistry struct{ lanes []CanonicalLane }

// NewCanonicalLaneRegistry builds the validated lane registry. Lane NAMES
// must be unique (a duplicate name is a config error). A ROLE may be owned by
// several lanes — standing improvement fleets run ci-warden and mender both
// carrying, say, "assayer" — but ResolveRole then refuses to pick one
// silently and names every candidate instead, exactly like
// config.ResolveLane does at the library boundary.
func NewCanonicalLaneRegistry(lanes []CanonicalLane) (CanonicalLaneRegistry, error) {
	seenName := map[string]bool{}
	copyLanes := make([]CanonicalLane, 0, len(lanes))
	for _, lane := range lanes {
		name, role := strings.TrimSpace(lane.Name), strings.TrimSpace(lane.Role)
		if name == "" || role == "" || seenName[strings.ToLower(name)] {
			return CanonicalLaneRegistry{}, fmt.Errorf("invalid or duplicate canonical lane %q", name)
		}
		seenName[strings.ToLower(name)] = true
		copyLanes = append(copyLanes, CanonicalLane{Name: name, Role: role, Standing: lane.Standing})
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
	role = strings.TrimSpace(role)
	var matches []CanonicalLane
	for _, lane := range r.lanes {
		if strings.EqualFold(lane.Role, role) {
			matches = append(matches, lane)
		}
	}
	switch len(matches) {
	case 0:
		return CanonicalLane{}, fmt.Errorf("unknown role %q", role)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return CanonicalLane{}, fmt.Errorf("%w: role %q is held by %d lanes (%v) — name one of them explicitly", ErrAmbiguousRole, role, len(matches), names)
	}
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
	if nameErr == nil {
		return byName, nil
	}
	byRole, roleErr := r.ResolveRole(value)
	if roleErr == nil {
		return byRole, nil
	}
	// nameErr != nil. If the role matched multiple lanes, propagate the
	// ambiguity error — it names the candidates so the caller can
	// disambiguate by naming a lane. Otherwise neither lookup matched.
	if errors.Is(roleErr, ErrAmbiguousRole) {
		return CanonicalLane{}, roleErr
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
