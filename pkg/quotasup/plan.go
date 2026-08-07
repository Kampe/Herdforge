package quotasup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// BurnFor resolves the ledger row for one surface.
//
// A provider the ledger does not break into pools (grok, kimi) meters
// everything through its single top-level row, so that row IS the pool's row.
// A provider that DOES break into pools and has no row for this one has told
// us nothing about it: return nil so it grades Untracked. Substituting the
// provider aggregate there would let a busy sibling's burn masquerade as this
// pool's, which is the mis-billing this package exists to avoid.
func BurnFor(computed map[string]usage.BurnState, s Surface) *usage.BurnState {
	prov, ok := computed[s.Provider]
	if !ok {
		return nil
	}
	if len(prov.Pools) == 0 {
		top := prov
		return &top
	}
	if row, ok := prov.Pools[s.Pool]; ok {
		return &row
	}
	return nil
}

// Surfaces enumerates every surface worth a decision: those the ledger
// describes, plus any surface a live agent is running on. The second set
// matters because an agent on a surface the ledger never mentions is exactly
// the case that must grade Untracked rather than be quietly skipped.
func Surfaces(computed map[string]usage.BurnState, live []Surface) []Surface {
	seen := make(map[Surface]bool)
	var out []Surface
	add := func(s Surface) {
		if s.Provider == "" {
			return
		}
		if s.Pool == "" {
			s.Pool = "default"
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for provider, prov := range computed {
		if len(prov.Pools) == 0 {
			add(Surface{Provider: provider, Pool: "default"})
			continue
		}
		for pool := range prov.Pools {
			add(Surface{Provider: provider, Pool: pool})
		}
	}
	for _, s := range live {
		add(s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Pool < out[j].Pool
	})
	return out
}

// PriorState returns a surface's persisted cap and streak, or the cold-start
// zero value. Cold start is deliberately {0, 0}: with no memory of what the
// fleet was doing, the supervisor starts blocked and climbs under the same
// verified-recovery rule as everything else, rather than assuming headroom it
// has not observed twice.
func PriorState(prev *Snapshot, s Surface) State {
	if prev == nil {
		return State{}
	}
	for _, d := range prev.Decisions {
		if d.Surface == s {
			return d.State()
		}
	}
	return State{}
}

// RouteProviders maps a quota-ledger provider to the pkg/router provider names
// that meter through it, mirroring SurfaceRouter.quotaState's aliasing. A cool
// written under the ledger's own name would never match the router's query for
// "agy" or "ollama", and would gate nothing.
func RouteProviders(ledgerProvider string) []string {
	switch ledgerProvider {
	case "antigravity":
		return []string{"agy"}
	case "opencode":
		return []string{"opencode", "ollama", "lazer"}
	}
	return []string{ledgerProvider}
}

// SurfaceCooldown reports the cool already blocking a surface, through the
// router's own reader, so the supervisor grades against the gate it exists to
// anticipate. Every router name metering through the ledger provider is
// checked, since a cool on any of them gates launches that bill this pool.
//
// The supervisor's OWN cools are skipped. They are a consequence of a previous
// decision, not independent evidence, and reading them back would latch the
// surface shut: --act blocks, the next run sees the block it wrote, blocks
// again, and no amount of recovered quota ever lifts it. A hand-written or
// third-party hold carries no ActSource and does count.
func SurfaceCooldown(now time.Time, s Surface) string {
	for _, rp := range RouteProviders(s.Provider) {
		if c := router.CooldownFor(now, rp, "", s.Pool); c != nil && c.Source != ActSource {
			return c.Reason
		}
	}
	return ""
}

// ActSource marks the cooldown entries this supervisor wrote. --act removes
// only entries carrying it: a human's manual hold is not the supervisor's to
// clear just because quota looks fine again.
const ActSource = "herd-quota-supervisor"

const (
	// MinActCooldown / MaxActCooldown bound every cool --act writes. The
	// upper bound is what makes --act safe to run unattended: if the
	// supervisor dies holding a block, the block expires on its own instead
	// of stranding the surface until someone notices.
	MinActCooldown = 5 * time.Minute
	MaxActCooldown = 30 * time.Minute
)

// CooldownPath is the pool-scoped file pkg/router.CooldownReason reads.
//
// Pool-scoped, always. The bare <provider>.cooldown.json is a provider-wide
// gate that would take every sibling pool down with the blocked one.
func CooldownPath(dir, routeProvider, pool string) string {
	return filepath.Join(dir, routeProvider+"--"+pool+".cooldown.json")
}

type cooldownEntry struct {
	Provider  string `json:"provider"`
	Pool      string `json:"pool"`
	ExpiresAt int64  `json:"expiresAt"`
	Reason    string `json:"reason"`
	Source    string `json:"source"`
}

// Act reconciles the routing cooldown store with the supervisor's decisions:
// blocked surfaces get a bounded, pool-scoped cool; every other surface has
// the supervisor's own cool lifted. It returns one line per change made.
//
// ttl is clamped to [MinActCooldown, MaxActCooldown]; a caller cannot widen
// the blast radius of an automated block by passing a large value.
//
// The directory is router.GlobalStateDir() and is deliberately NOT a
// parameter. Act has to read the store (to see whose cool is already there)
// as well as write it, and a caller that could point those at different
// directories would get a supervisor that checks one store and stamps another.
func Act(now time.Time, ttl time.Duration, decisions []Decision) ([]string, error) {
	if ttl < MinActCooldown {
		ttl = MinActCooldown
	}
	if ttl > MaxActCooldown {
		ttl = MaxActCooldown
	}
	dir := router.GlobalStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("quota supervisor act: create routing state dir: %w", err)
	}

	var changes []string
	for _, d := range decisions {
		for _, rp := range RouteProviders(d.Surface.Provider) {
			path := CooldownPath(dir, rp, d.Surface.Pool)
			if d.Posture == PostureBlocked {
				// Someone else's live hold already gates this surface. Do not
				// restamp it as ours: that both shortens their deadline to our
				// clamped ttl AND hands us the right to lift it on the next
				// tick, so two ordinary ticks would silently destroy a manual
				// hold. Their block and ours have the same effect, so there is
				// nothing to gain by writing.
				if reason, held := foreignGate(now, rp, d.Surface.Pool); held {
					changes = append(changes, fmt.Sprintf("deferred %s/%s to an existing hold: %s",
						rp, d.Surface.Pool, reason))
					continue
				}
				body, err := json.Marshal(cooldownEntry{
					Provider:  rp,
					Pool:      d.Surface.Pool,
					ExpiresAt: now.Add(ttl).Unix(),
					Reason:    d.Reason,
					Source:    ActSource,
				})
				if err != nil {
					return changes, fmt.Errorf("quota supervisor act: marshal cool for %s: %w", d.Surface, err)
				}
				if err := writeFileAtomic(path, body); err != nil {
					return changes, err
				}
				changes = append(changes, fmt.Sprintf("cooled %s/%s for %s: %s",
					rp, d.Surface.Pool, ttl, d.Reason))
				continue
			}
			lifted, err := clearOwnCooldown(path)
			if err != nil {
				return changes, err
			}
			if lifted {
				changes = append(changes, fmt.Sprintf("lifted %s/%s: %s", rp, d.Surface.Pool, d.Reason))
			}
		}
	}
	return changes, nil
}

// foreignGate reports a live cool on this surface that the supervisor did not
// write.
//
// It asks router.CooldownFor rather than reading the file, so "is this surface
// already held" is answered by the same scoping and expiry rules the launch
// gate applies. A file that sits at our path but does not actually gate this
// surface — wrong provider, wrong pool, expired, unparseable — is not a hold,
// and refusing to write over it would leave an exhausted surface ungated.
func foreignGate(now time.Time, routeProvider, pool string) (string, bool) {
	c := router.CooldownFor(now, routeProvider, "", pool)
	if c == nil || c.Source == ActSource {
		return "", false
	}
	return c.Reason, true
}

// clearOwnCooldown removes a cool only if this supervisor wrote it. An
// unreadable or foreign entry is left exactly where it is.
func clearOwnCooldown(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil // absent, or not ours to read
	}
	var e cooldownEntry
	if json.Unmarshal(raw, &e) != nil || e.Source != ActSource {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("quota supervisor act: lift cool %s: %w", path, err)
	}
	return true, nil
}

func writeFileAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("quota supervisor act: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("quota supervisor act: install %s: %w", path, err)
	}
	return nil
}
