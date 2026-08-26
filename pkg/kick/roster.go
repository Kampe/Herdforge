package kick

import (
	"os"
	"sort"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/resolve"
)

// ForgePrefix is prepended to every lane id when deriving the live agent
// name for that lane (main.go, cleanup, and the spawn path all converge on
// "forge-<lane>" for standing agent names), so the roster produced here must
// match the live fleet the same way.
const ForgePrefix = "forge-"

// registryPaths is the search order for the lane registry JSON. Order
// matches main.go runResolveLane so the roster resolves consistently.
var registryPaths = []string{
	"docs/agent/lane-registry.json",
	".herd/lane-registry.json",
}

// standingOverride pins the standing roster for tests. nil restores derivation
// from the repo registry / herd.yaml. When set, each entry is assumed to be a
// final live agent name (already prefixed); no additional prefixing is applied.
var standingOverride []string

// SetStandingOverride replaces the standing roster with the given live agent
// names (tests pass e.g. "forge-worker"), overriding derivation. Pass nil to
// restore derivation.
func SetStandingOverride(ids []string) { standingOverride = ids }

// LaneIDs returns the sorted, de-duplicated lane ids this repo declares:
// the lane registry (docs/agent/lane-registry.json, then
// .herd/lane-registry.json) via resolve.ParseRegistry, falling back to the
// lanes in .herd/herd.yaml. Returns an empty slice when no source is found.
func LaneIDs() []string {
	if data := readRegistry(); data != nil {
		if reg, err := resolve.ParseRegistry(data); err == nil && len(reg.Lanes) > 0 {
			ids := make([]string, 0, len(reg.Lanes))
			for _, l := range reg.Lanes {
				ids = append(ids, l.ID)
			}
			return sortedUnique(ids)
		}
	}
	if cfg, err := config.LoadConfig(config.DefaultConfigPath); err == nil {
		ids := make([]string, 0, len(cfg.Lanes))
		for _, l := range cfg.Lanes {
			ids = append(ids, l.Name)
		}
		return sortedUnique(ids)
	}
	return nil
}

// StandingIDs returns the canonical standing fleet roster for this repo:
// the ForgePrefix-prefixed id (e.g. lane "worker" -> "forge-worker") of
// every lane declared standing:true, sorted and de-duplicated. Ephemeral
// task roles (standing:false/omitted — worker, reviewer, verification-gate,
// …) are launched per dispatch, not raised here. This is the roster kick,
// attention, and cleanup use to match live herdr agents. A test override
// set via SetStandingOverride returns those names verbatim.
func StandingIDs() []string {
	if standingOverride != nil {
		return sortedUnique(append([]string(nil), standingOverride...))
	}
	ids := make([]string, 0)
	if data := readRegistry(); data != nil {
		if reg, err := resolve.ParseRegistry(data); err == nil && len(reg.Lanes) > 0 {
			for _, l := range reg.Lanes {
				if l.Standing {
					ids = append(ids, ForgePrefix+l.ID)
				}
			}
			// FAC-660: a registry that lists lanes but marks NONE of them
			// standing has not told us the roster is empty -- it has told us it
			// does not carry standing flags. Returning here treated the second
			// as the first.
			//
			// Measured live: docs/agent/lane-registry.json holds 14 lanes with 0
			// standing, while .herd/herd.yaml beside it marks 14 standing. The
			// early return meant the config was never read, so the roster came
			// back empty, attention scanned nothing and reported UNKNOWN with a
			// full fleet running, and the reaper saw no lanes to protect.
			//
			// A registry that DOES mark standing lanes is still authoritative and
			// still returns here; only the zero case falls through to the config.
			if len(ids) > 0 {
				return sortedUnique(ids)
			}
		}
	}
	if cfg, err := config.LoadConfig(config.DefaultConfigPath); err == nil {
		for _, l := range cfg.Lanes {
			if l.Standing {
				ids = append(ids, ForgePrefix+l.Name)
			}
		}
		return sortedUnique(ids)
	}
	return nil
}

func readRegistry() []byte {
	for _, p := range registryPaths {
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
	}
	return nil
}

func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	out := ids[:1]
	for _, id := range ids[1:] {
		if id != out[len(out)-1] {
			out = append(out, id)
		}
	}
	return out
}
