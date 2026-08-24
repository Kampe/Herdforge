package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// liveHarnessProof, when set (by herdr.RegisterLiveHarnessProof or tests), is
// the only function that may return RealHerdrSession=true.
var (
	liveProofMu      sync.RWMutex
	liveHarnessProof func(kind, realBin, tmp string) (modelOK, toolOK, viaLA, contained, herdrOK bool, evidence, blocker string, err error)
)

// RegisterLiveHarnessProof installs the production live-Herdr proof driver.
// Called from pkg/herdr init so security does not import herdr (cycle).
func RegisterLiveHarnessProof(fn func(kind, realBin, tmp string) (modelOK, toolOK, viaLA, contained, herdrOK bool, evidence, blocker string, err error)) {
	liveProofMu.Lock()
	liveHarnessProof = fn
	liveProofMu.Unlock()
}

// proveLiveHerdrSession is the only path that may mark a harness Usable.
func proveLiveHerdrSession(kind, realBin, tmp string) (modelOK, toolOK, viaLA, contained, herdrOK bool, evidence, blocker string, err error) {
	liveProofMu.RLock()
	fn := liveHarnessProof
	liveProofMu.RUnlock()
	if fn == nil {
		return false, false, false, false, false, "",
			fmt.Sprintf("FAC-133 BLOCKED: live Herdr proof driver not registered for %s "+
				"(herdr package must RegisterLiveHarnessProof; synthetic probes forbidden)", kind),
			fmt.Errorf("live herdr proof driver not registered")
	}
	return fn(kind, realBin, tmp)
}

// AssertNotSyntheticallyUsable is a mutation guard: results that claim usable
// without RealHerdrSession must fail closed.
func AssertNotSyntheticallyUsable(r *HarnessProbeResult) error {
	if r == nil {
		return fmt.Errorf("nil result")
	}
	if r.Usable && !r.RealHerdrSession {
		return fmt.Errorf("FAC-133: usable without RealHerdrSession is synthetic/forgeable")
	}
	if r.Usable && (strings.Contains(r.ToolEvidence, "tab-real") || strings.Contains(r.ToolEvidence, "ses_real_")) {
		return fmt.Errorf("FAC-133: synthetic session evidence")
	}
	if r.Usable && strings.Contains(r.ToolEvidence, "parent-wrote-sentinel") {
		return fmt.Errorf("FAC-133: parent-written tool sentinel is vacuous")
	}
	if r.Usable && strings.HasPrefix(r.ToolEvidence, "pending-") {
		return fmt.Errorf("FAC-133: provisional session evidence")
	}
	return nil
}

// WaitForAgentFile waits for a path to appear (agent-written tool artifact).
func WaitForAgentFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for agent file %s", path)
}

// ScratchWorktree creates a hermetic worktree dir under tmp for live proofs.
func ScratchWorktree(tmp string) (shared, wt string, err error) {
	shared = filepath.Join(tmp, "shared")
	wt = filepath.Join(shared, "wt")
	if err := os.MkdirAll(filepath.Join(wt, ".tmp"), 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Join(shared, ".herd"), 0o755); err != nil {
		return "", "", err
	}
	return shared, wt, nil
}
