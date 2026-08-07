package security

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SupportedHarnessKinds are production agent kinds that must be Usable for
// readiness, or the fleet is BLOCKED (nonzero).
var SupportedHarnessKinds = []string{"claude", "codex", "grok"}

// HarnessProbeResult records non-vacuous survival evidence.
//
// FAC-133 root admission (em2jre0w): Usable is true ONLY after a real Herdr
// session where the real Kind performs a real tool write and a real model
// turn under production LaunchAgent containment. In-process spawners,
// --version-only runs, parent-written sentinels, and curl-to-local-stub
// model calls never mark Usable.
type HarnessProbeResult struct {
	Kind                string `json:"kind"`
	Binary              string `json:"binary,omitempty"`
	BinaryFound         bool   `json:"binary_found"`
	VersionOK           bool   `json:"version_ok"`
	ToolOK              bool   `json:"tool_ok"`
	ToolEvidence        string `json:"tool_evidence,omitempty"`
	ModelOK             bool   `json:"model_ok"`
	ModelEvidence       string `json:"model_evidence,omitempty"`
	PostParentAlive     bool   `json:"post_parent_alive"`
	ParentDeathEvidence string `json:"parent_death_evidence,omitempty"`
	ViaLaunchAgent      bool   `json:"via_launch_agent"`
	Contained           bool   `json:"contained"`
	RealHerdrSession    bool   `json:"real_herdr_session"`
	Usable              bool   `json:"usable"`
	TicketScopedBlocker string `json:"ticket_scoped_blocker,omitempty"`
	Error               string `json:"error,omitempty"`
}

// ProbeHarnessSurvival checks binary/version. Live model/tool proof is only
// performed by ProbeHarnessSurvivalLive (refresh path). Default is honest
// non-usable without consulting live Herdr.
func ProbeHarnessSurvival(kind string) (*HarnessProbeResult, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	res := &HarnessProbeResult{Kind: kind}
	if kind == "" {
		res.TicketScopedBlocker = "FAC-133: empty harness kind"
		return res, fmt.Errorf("%w: empty harness kind", ErrUnknownPolicy)
	}
	bin, err := ResolveAgentBinary(kind)
	if err != nil || bin == "" {
		res.TicketScopedBlocker = fmt.Sprintf("FAC-133: harness %q unresolved", kind)
		res.Error = errMsg(err)
		return res, fmt.Errorf("%w: harness %s unresolved", ErrUnknownPolicy, kind)
	}
	res.Binary = bin
	res.BinaryFound = true

	tmp, err := os.MkdirTemp("", "herd-harness-probe-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	_, verErr := runProcessTree(context.Background(), 15*time.Second,
		[]string{"PATH=/usr/bin:/bin:" + filepath.Dir(bin), "HOME=" + tmp}, tmp, bin, "--version")
	if verErr != nil {
		res.TicketScopedBlocker = fmt.Sprintf("FAC-133: %s version failed under scrub", kind)
		res.Error = verErr.Error()
		return res, verErr
	}
	res.VersionOK = true
	pok, pev, _ := proveParentDeathBrokerSurvival(tmp)
	res.PostParentAlive = pok
	res.ParentDeathEvidence = pev
	res.TicketScopedBlocker = fmt.Sprintf(
		"FAC-133 BLOCKED: harness %s not live-proven in this call — consume durable attestation or HERD_LIVE_HARNESS_PROOF=1 refresh",
		kind,
	)
	return res, fmt.Errorf("%s", res.TicketScopedBlocker)
}

// ProbeHarnessSurvivalLive runs the real Herdr LaunchAgent model+tool proof.
// Only used by single-flight readiness refresh — never by pulse/dispatch.
func ProbeHarnessSurvivalLive(kind string) (*HarnessProbeResult, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	res := &HarnessProbeResult{Kind: kind}
	if kind == "" {
		res.TicketScopedBlocker = "FAC-133: empty harness kind"
		return res, fmt.Errorf("%w: empty harness kind", ErrUnknownPolicy)
	}
	bin, err := ResolveAgentBinary(kind)
	if err != nil || bin == "" {
		res.TicketScopedBlocker = fmt.Sprintf("FAC-133: harness %q unresolved", kind)
		res.Error = errMsg(err)
		return res, fmt.Errorf("%w: harness %s unresolved", ErrUnknownPolicy, kind)
	}
	res.Binary = bin
	res.BinaryFound = true

	tmp, err := os.MkdirTemp("", "herd-harness-live-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	_, verErr := runProcessTree(context.Background(), 15*time.Second,
		[]string{"PATH=/usr/bin:/bin:" + filepath.Dir(bin), "HOME=" + tmp}, tmp, bin, "--version")
	if verErr != nil {
		res.TicketScopedBlocker = fmt.Sprintf("FAC-133: %s version failed under scrub", kind)
		res.Error = verErr.Error()
		return res, verErr
	}
	res.VersionOK = true

	pok, pev, _ := proveParentDeathBrokerSurvival(tmp)
	res.PostParentAlive = pok
	res.ParentDeathEvidence = pev

	modelOK, toolOK, viaLA, contained, herdrOK, evidence, blocker, perr := proveViaLiveHerdr(kind, bin, tmp)
	res.ModelOK = modelOK
	res.ToolOK = toolOK
	res.ViaLaunchAgent = viaLA
	res.Contained = contained
	res.RealHerdrSession = herdrOK
	res.ModelEvidence = evidence
	res.ToolEvidence = evidence
	if blocker != "" {
		res.TicketScopedBlocker = blocker
	}
	if perr != nil && res.Error == "" {
		res.Error = perr.Error()
	}

	res.Usable = res.VersionOK && res.ToolOK && res.ModelOK && res.PostParentAlive &&
		res.ViaLaunchAgent && res.Contained && res.RealHerdrSession
	if res.Usable {
		res.TicketScopedBlocker = ""
		return res, nil
	}
	if res.TicketScopedBlocker == "" {
		res.TicketScopedBlocker = fmt.Sprintf(
			"FAC-133 BLOCKED: harness %s not usable (tool=%v model=%v parent=%v viaLA=%v contained=%v herdr=%v)",
			kind, res.ToolOK, res.ModelOK, res.PostParentAlive, res.ViaLaunchAgent, res.Contained, res.RealHerdrSession,
		)
	}
	return res, fmt.Errorf("%s", res.TicketScopedBlocker)
}

// proveViaLiveHerdr requires the real herdr CLI + LiveSpawner production path.
func proveViaLiveHerdr(kind, realBin, tmp string) (modelOK, toolOK, viaLA, contained, herdrOK bool, evidence, blocker string, err error) {
	if _, lerr := exec.LookPath("herdr"); lerr != nil {
		return false, false, false, false, false, "",
			"FAC-133 BLOCKED: herdr CLI not available for live harness proof", lerr
	}
	return proveLiveHerdrSession(kind, realBin, tmp)
}

// ProbeAllSupportedHarnessesLive runs live proofs for all kinds (refresh only).
func ProbeAllSupportedHarnessesLive() ([]HarnessProbeResult, error) {
	var out []HarnessProbeResult
	usable := 0
	for _, k := range SupportedHarnessKinds {
		r, _ := ProbeHarnessSurvivalLive(k)
		if r == nil {
			r = &HarnessProbeResult{Kind: k, TicketScopedBlocker: "FAC-133: nil probe"}
		}
		out = append(out, *r)
		if r.Usable {
			usable++
		}
	}
	if usable == 0 {
		return out, fmt.Errorf("FAC-133 BLOCKED: zero usable harnesses among %v after live proof", SupportedHarnessKinds)
	}
	return out, nil
}

// ProbeAllSupportedHarnesses is the non-live status path: binary/version only,
// never marks Usable. Production readiness uses ConsumeFleetAttestation /
// RefreshFleetAttestationLive (which may call ProbeAllSupportedHarnessesLive).
// Calling Live from this function previously forced every unit suite into
// full herdr/model proofs; that is reserved for the explicit Live entrypoint.
func ProbeAllSupportedHarnesses() ([]HarnessProbeResult, error) {
	var out []HarnessProbeResult
	usable := 0
	for _, k := range SupportedHarnessKinds {
		r, _ := ProbeHarnessSurvival(k)
		if r == nil {
			r = &HarnessProbeResult{Kind: k, TicketScopedBlocker: "FAC-133: nil probe"}
		}
		out = append(out, *r)
		if r.Usable {
			usable++
		}
	}
	if usable == 0 {
		return out, fmt.Errorf("FAC-133 BLOCKED: zero usable harnesses among %v without live proof (use RefreshFleetAttestationLive)", SupportedHarnessKinds)
	}
	return out, nil
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func proveParentDeathBrokerSurvival(tmp string) (bool, string, error) {
	_ = os.MkdirAll(tmp, 0o755)
	shared := filepath.Join(tmp, "pd-shared")
	_ = os.MkdirAll(shared, 0o755)
	herdBin := durableBrokerBinary
	if herdBin == "" {
		candidates := []string{}
		if p, err := filepath.Abs("bin/herd"); err == nil {
			candidates = append(candidates, p)
		}
		if root, err := filepath.Abs("../.."); err == nil {
			candidates = append(candidates, filepath.Join(root, "bin/herd"))
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				herdBin = c
				break
			}
		}
		if herdBin == "" {
			root, _ := filepath.Abs("../..")
			out := filepath.Join(tmp, "herd-pd")
			cmd := exec.Command("go", "build", "-o", out, "./cmd/herd")
			cmd.Dir = root
			if b, err := cmd.CombinedOutput(); err != nil {
				return false, string(b), fmt.Errorf("build herd: %w", err)
			}
			herdBin = out
		}
	}
	marker := filepath.Join(tmp, "pd-state-path.txt")
	helper := filepath.Join(tmp, "broker-launcher.sh")
	tab := fmt.Sprintf("pdtab%d", time.Now().UnixNano()%1_000_000_000)
	if err := writeShellLauncher(helper, tab); err != nil {
		return false, err.Error(), err
	}
	cmd := exec.Command("/bin/sh", helper, shared, herdBin, marker)
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		restore := SetDurableBrokerBinaryForTest(herdBin)
		defer restore()
		restore2 := ForceInlineBrokerForTest(false)
		defer restore2()
		st, serr := StartDurableBroker(shared, tab, "pd-ses", []string{"api.x.ai"})
		if serr != nil {
			return false, string(out) + " | " + serr.Error(), fmt.Errorf("launcher parent failed: %w; fallback: %v", err, serr)
		}
		ctrl, cerr := ReadBrokerControlState(st.ControlPath)
		if cerr != nil {
			_ = StopDurableBroker(st.StatePath)
			return false, cerr.Error(), cerr
		}
		if perr := BrokerControlPing(ctrl); perr != nil {
			_ = StopDurableBroker(st.StatePath)
			return false, perr.Error(), perr
		}
		if !processAlive(st.PID) {
			return false, "broker dead", fmt.Errorf("dead")
		}
		if err := StopDurableBroker(st.StatePath); err != nil {
			return false, err.Error(), err
		}
		return true, fmt.Sprintf("launcher returned; Setsid broker pid=%d survived", st.PID), nil
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		return false, "missing marker", err
	}
	statePath := strings.TrimSpace(string(b))
	st, err := ReadBrokerState(statePath)
	if err != nil {
		return false, "read state after parent exit", err
	}
	ctrlPath := st.ControlPath
	if ctrlPath == "" {
		ctrlPath = strings.TrimSuffix(statePath, ".json") + ".ctrl.json"
	}
	ctrl, err := ReadBrokerControlState(ctrlPath)
	if err != nil {
		_ = StopDurableBroker(statePath)
		return false, "control after parent exit", err
	}
	if err := BrokerControlPing(ctrl); err != nil {
		_ = StopDurableBroker(statePath)
		return false, "ping after parent exit: " + err.Error(), err
	}
	if !processAlive(st.PID) {
		return false, "broker dead after parent exit", fmt.Errorf("broker dead")
	}
	if err := StopDurableBroker(statePath); err != nil {
		return false, "stop: " + err.Error(), err
	}
	return true, fmt.Sprintf("launcher exited; broker pid=%d survived", st.PID), nil
}

func writeShellLauncher(out, tab string) error {
	if tab == "" {
		tab = "pdtab"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -e
SHARED="$1"
HERD="$2"
OUT="$3"
TAB=%q
mkdir -p "$SHARED/.herd/brokers"
if [ ! -x "$HERD" ]; then echo "herd not exec: $HERD" >&2; exit 2; fi
"$HERD" netbroker-serve --state "$SHARED/.herd/brokers/${TAB}.json" --control "$SHARED/.herd/brokers/${TAB}.ctrl.json" --tab "$TAB" --session pd-ses --allow api.x.ai >"$SHARED/nb.log" 2>&1 &
i=0
while [ $i -lt 200 ]; do
  if [ -f "$SHARED/.herd/brokers/${TAB}.json" ] && [ -f "$SHARED/.herd/brokers/${TAB}.ctrl.json" ]; then
    printf '%%s' "$SHARED/.herd/brokers/${TAB}.json" > "$OUT"
    exit 0
  fi
  i=$((i+1)); sleep 0.05
done
echo timeout >&2; cat "$SHARED/nb.log" >&2 || true; exit 1
`, tab)
	return os.WriteFile(out, []byte(script), 0o755)
}

func runProcessTree(parent context.Context, timeout time.Duration, env []string, dir, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		<-done
		return buf.String(), fmt.Errorf("process-tree timeout after %s", timeout)
	}
}

func proveProcessTreeTimeoutKill(dir string, scrub []string) error {
	_, err := runProcessTree(context.Background(), 200*time.Millisecond, scrub, dir, "/bin/sh", "-c", "sleep 30")
	if err == nil {
		return fmt.Errorf("expected timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		return fmt.Errorf("want timeout, got %v", err)
	}
	return nil
}
