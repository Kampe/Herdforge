package config

import "fmt"

// ResolveLane finds the lane a caller named, accepting either a lane NAME or a
// ROLE.
//
// It exists because the dispatch default is "worker", which is a role — no lane
// is named that — so a bare `herd dispatch <REF>` died at lane lookup before it
// reached the dependency check. Two independent lookups had to agree for the
// launch path to work (pkg/dispatch and cmd/herd's findLaneByName); fixing only
// one left real launches still failing while `--no-launch` appeared to work,
// because --no-launch skips the CLI lookup entirely.
//
// The CLI no longer calls this. It resolves once through CanonicalLaneRegistry
// and passes the resolved lane NAME downstream, so the hold gate and the launch
// bind the same lane by construction rather than by two lookups agreeing. This
// remains the resolution for library callers of dispatch.Dispatch, which receive
// a bare string and may legitimately pass a role.
//
// Name wins over role, so an explicit lane name is never overridden.
//
// A role match must be UNIQUE. config.Validate only rejects duplicate STANDING
// role owners, so two ephemeral lanes may share a role at this layer; picking
// the first in file order would silently bind a caller to a lane it did not ask
// for. Ambiguity is an error naming both candidates, not a coin flip.
//
// The CLI additionally builds a CanonicalLaneRegistry, which rejects ANY
// duplicate role, so no shipping CLI path can reach the ambiguous case today.
// That makes this a library-boundary guard, not a live defence — kept because
// dispatch.Dispatch is callable without that registry, and cheap to hold.
func ResolveLane(cfg *Config, nameOrRole string) (*LaneDef, error) {
	if cfg == nil {
		return nil, fmt.Errorf("lane %q not resolvable: no config", nameOrRole)
	}
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Name == nameOrRole {
			return &cfg.Lanes[i], nil
		}
	}
	var matches []int
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Role == nameOrRole {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("lane %q not found in config (no lane with that name, and no lane with that role)", nameOrRole)
	case 1:
		return &cfg.Lanes[matches[0]], nil
	default:
		names := make([]string, 0, len(matches))
		for _, i := range matches {
			names = append(names, cfg.Lanes[i].Name)
		}
		return nil, fmt.Errorf("lane %q is ambiguous: role %q is held by %d lanes (%v) — name one of them explicitly",
			nameOrRole, nameOrRole, len(matches), names)
	}
}
