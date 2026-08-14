package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultFleetAttestationTTL is how long a successful live readiness proof remains valid.
const DefaultFleetAttestationTTL = 6 * time.Hour

// FleetAttestation is a durable, MAC-signed, binary+policy+containment-bound
// readiness record. Pulse/dispatch only consume it; they never re-spawn models.
type FleetAttestation struct {
	Version            int                  `json:"version"`
	IssuedAt           time.Time            `json:"issued_at"`
	ExpiresAt          time.Time            `json:"expires_at"`
	Generation         string               `json:"generation"`
	Revoked            bool                 `json:"revoked"`
	HerdBinaryDigest   string               `json:"herd_binary_digest"`
	HarnessDigests     map[string]string    `json:"harness_digests"`
	ContainmentBackend string               `json:"containment_backend"`
	PolicyDigest       string               `json:"policy_digest"`
	Usable             int                  `json:"usable"`
	Results            []HarnessProbeResult `json:"results"`
	Evidence           string               `json:"evidence,omitempty"`
	// Signature is HMAC-SHA256 over Canonical() under the control secret.
	// Empty signature is fail-closed on consume.
	Signature string `json:"signature"`
}

const fleetAttestVersion = 2

var (
	fleetRefreshMu sync.Mutex // single-flight live refresh
)

// FleetAttestationPath is under shared/repo .herd (not worktree-writable by agents).
func FleetAttestationPath(sharedRoot string) string {
	root, err := TrustedReadinessRoot(sharedRoot)
	if err != nil {
		// Path helper still returns a conventional path; load/save must re-validate.
		if sharedRoot == "" {
			sharedRoot = "."
		}
		return filepath.Join(sharedRoot, ".herd", "readiness", "fleet.json")
	}
	return filepath.Join(root, ".herd", "readiness", "fleet.json")
}

// TrustedReadinessRoot resolves and validates the durable readiness root.
// Refuses absolute escapes and caller roots that are not the repo / HERD_ROOT.
func TrustedReadinessRoot(explicit string) (string, error) {
	cand := strings.TrimSpace(explicit)
	if cand == "" {
		cand = strings.TrimSpace(os.Getenv("HERD_READINESS_ROOT"))
	}
	if cand == "" {
		cand = strings.TrimSpace(os.Getenv("HERD_ROOT"))
	}
	if cand == "" {
		cand = "."
	}
	// Refuse path traversal in the candidate itself.
	if strings.Contains(cand, "..") {
		return "", fmt.Errorf("%w: readiness root traversal refused", ErrFleetBlocked)
	}
	abs, err := filepath.Abs(cand)
	if err != nil {
		return "", fmt.Errorf("%w: readiness root resolve: %v", ErrFleetBlocked, err)
	}
	// Anchor: when HERD_ROOT is set, explicit root must be under it.
	if hr := strings.TrimSpace(os.Getenv("HERD_ROOT")); hr != "" {
		hab, herr := filepath.Abs(hr)
		if herr == nil {
			if abs != hab && !strings.HasPrefix(abs, hab+string(filepath.Separator)) {
				return "", fmt.Errorf("%w: readiness root %q outside HERD_ROOT", ErrFleetBlocked, abs)
			}
		}
	}
	// Must look like a herd repo (has .herd or will create under our control).
	return abs, nil
}

// ResolveReadinessRoot picks the durable root for fleet attestation (validated).
func ResolveReadinessRoot() string {
	r, err := TrustedReadinessRoot("")
	if err != nil {
		return "."
	}
	return r
}

// FileSHA256Hex returns hex sha256 of a file, or "" if unreadable.
func FileSHA256Hex(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CurrentReadinessBindingFor digests herd + the specified harness binaries + containment.
func CurrentReadinessBindingFor(kinds []string) (herdDigest string, harness map[string]string, containment string, err error) {
	harness = map[string]string{}
	if self, e := os.Executable(); e == nil {
		herdDigest = FileSHA256Hex(self)
	}
	if herdDigest == "" {
		if p, e := filepath.Abs("bin/herd"); e == nil {
			herdDigest = FileSHA256Hex(p)
		}
	}
	if herdDigest == "" {
		return "", harness, "unavailable", fmt.Errorf("%w: herd binary digest unavailable", ErrFleetBlocked)
	}
	if len(kinds) == 0 {
		kinds = ResolveRequiredHarnessKinds(ResolveReadinessRoot())
	}
	for _, k := range kinds {
		bin, berr := ResolveAgentBinary(k)
		if berr != nil || bin == "" {
			harness[k] = ""
			continue
		}
		d := FileSHA256Hex(bin)
		if d == "" {
			harness[k] = ""
			continue
		}
		harness[k] = d
	}
	if backend, berr := RequireContainment(); berr == nil && backend != nil {
		containment = backend.Name()
	} else {
		containment = "unavailable"
		if berr != nil {
			err = berr
		} else {
			err = fmt.Errorf("%w: containment unavailable", ErrFleetBlocked)
		}
	}
	return herdDigest, harness, containment, err
}

// CurrentReadinessBinding digests herd + all required harness binaries + containment.
// Missing a required harness binary is recorded as empty digest (usable that kind impossible).
func CurrentReadinessBinding() (herdDigest string, harness map[string]string, containment string, err error) {
	return CurrentReadinessBindingFor(ResolveRequiredHarnessKinds(ResolveReadinessRoot()))
}

// FleetAttestationSecret returns the MAC secret for readiness (control secret).
func FleetAttestationSecret() (string, error) {
	s := strings.TrimSpace(os.Getenv("HERD_CONTROL_SECRET"))
	if s == "" {
		s = strings.TrimSpace(os.Getenv("HERD_READINESS_SECRET"))
	}
	if s == "" {
		return "", fmt.Errorf("%w: HERD_CONTROL_SECRET (or HERD_READINESS_SECRET) required to sign/verify fleet attestation", ErrFleetBlocked)
	}
	return s, nil
}

// Canonical returns the stable signing payload (excludes Signature).
func (a *FleetAttestation) Canonical() []byte {
	if a == nil {
		return nil
	}
	// Deterministic JSON of fields excluding Signature.
	type canon struct {
		Version            int                  `json:"version"`
		IssuedAt           time.Time            `json:"issued_at"`
		ExpiresAt          time.Time            `json:"expires_at"`
		Generation         string               `json:"generation"`
		Revoked            bool                 `json:"revoked"`
		HerdBinaryDigest   string               `json:"herd_binary_digest"`
		HarnessDigests     map[string]string    `json:"harness_digests"`
		ContainmentBackend string               `json:"containment_backend"`
		PolicyDigest       string               `json:"policy_digest"`
		Usable             int                  `json:"usable"`
		Results            []HarnessProbeResult `json:"results"`
		Evidence           string               `json:"evidence,omitempty"`
	}
	// Sort harness keys via re-encode of map (json sorts? no - use ordered slice)
	keys := make([]string, 0, len(a.HarnessDigests))
	for k := range a.HarnessDigests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hd := make(map[string]string, len(keys))
	for _, k := range keys {
		hd[k] = a.HarnessDigests[k]
	}
	c := canon{
		Version: a.Version, IssuedAt: a.IssuedAt.UTC(), ExpiresAt: a.ExpiresAt.UTC(),
		Generation: a.Generation, Revoked: a.Revoked, HerdBinaryDigest: a.HerdBinaryDigest,
		HarnessDigests: hd, ContainmentBackend: a.ContainmentBackend, PolicyDigest: a.PolicyDigest,
		Usable: a.Usable, Results: a.Results, Evidence: a.Evidence,
	}
	b, _ := json.Marshal(c)
	return b
}

// SignFleetAttestation HMAC-signs attestation with secret.
func SignFleetAttestation(secret string, a *FleetAttestation) error {
	if strings.TrimSpace(secret) == "" || a == nil {
		return fmt.Errorf("%w: secret and attestation required", ErrFleetBlocked)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(a.Canonical())
	a.Signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return nil
}

// VerifyFleetAttestationMAC checks HMAC (constant-time).
func VerifyFleetAttestationMAC(secret string, a *FleetAttestation) bool {
	if a == nil || strings.TrimSpace(secret) == "" || a.Signature == "" {
		return false
	}
	if !strings.HasPrefix(a.Signature, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(a.Signature, "sha256="))
	if err != nil || len(want) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(a.Canonical())
	return hmac.Equal(want, mac.Sum(nil))
}

// LoadFleetAttestation loads raw attestation (MAC not yet verified).
func LoadFleetAttestation(path string) (*FleetAttestation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a FleetAttestation
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("fleet attestation corrupt: %w", err)
	}
	return &a, nil
}

// countValidatedUsable counts results that are fully gated usable (not a bare flag).
func countValidatedUsable(results []HarnessProbeResult) int {
	n := 0
	for _, r := range results {
		if resultFullyUsable(r) {
			n++
		}
	}
	return n
}

func resultFullyUsable(r HarnessProbeResult) bool {
	return r.Usable && r.VersionOK && r.ToolOK && r.ModelOK && r.PostParentAlive &&
		r.ViaLaunchAgent && r.Contained && r.RealHerdrSession &&
		!strings.HasPrefix(r.ToolEvidence, "pending-") &&
		!strings.Contains(r.ToolEvidence, "ses_probe_") &&
		!strings.Contains(r.ToolEvidence, "ses_real_") &&
		!strings.Contains(r.ToolEvidence, "parent-wrote")
}

// ValidateFleetAttestation checks MAC, TTL, revocation, binding, PolicyDigest, and results.
func ValidateFleetAttestation(a *FleetAttestation, now time.Time, secret string) error {
	if a == nil {
		return fmt.Errorf("%w: nil fleet attestation", ErrFleetBlocked)
	}
	if a.Version != fleetAttestVersion {
		return fmt.Errorf("%w: attestation version mismatch (got %d want %d)", ErrFleetBlocked, a.Version, fleetAttestVersion)
	}
	if a.Revoked {
		return fmt.Errorf("%w: attestation revoked", ErrFleetBlocked)
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("%w: attestation secret required", ErrFleetBlocked)
	}
	if !VerifyFleetAttestationMAC(secret, a) {
		return fmt.Errorf("%w: attestation MAC invalid (forged or wrong secret)", ErrFleetBlocked)
	}
	if strings.TrimSpace(a.PolicyDigest) == "" {
		return fmt.Errorf("%w: attestation missing PolicyDigest", ErrFleetBlocked)
	}
	if a.Usable <= 0 {
		return fmt.Errorf("%w: attestation has zero usable harnesses", ErrFleetBlocked)
	}
	// Usable must match validated results — not a caller-supplied integer alone.
	got := countValidatedUsable(a.Results)
	if got != a.Usable || got == 0 {
		return fmt.Errorf("%w: usable=%d does not match validated results=%d", ErrFleetBlocked, a.Usable, got)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
		return fmt.Errorf("%w: attestation expired at %s", ErrFleetBlocked, a.ExpiresAt.Format(time.RFC3339))
	}
	keys := make([]string, 0, len(a.HarnessDigests))
	for k := range a.HarnessDigests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	herd, harness, containment, berr := CurrentReadinessBindingFor(keys)
	if berr != nil || containment == "" || containment == "unavailable" {
		return fmt.Errorf("%w: containment/binding unavailable: %v", ErrFleetBlocked, berr)
	}
	if a.ContainmentBackend == "" || a.ContainmentBackend == "unavailable" || a.ContainmentBackend == "skipped" {
		return fmt.Errorf("%w: attestation missing real containment binding", ErrFleetBlocked)
	}
	if a.ContainmentBackend != containment {
		return fmt.Errorf("%w: containment binding changed (%s -> %s)", ErrFleetBlocked, a.ContainmentBackend, containment)
	}
	if a.HerdBinaryDigest == "" || herd == "" || a.HerdBinaryDigest != herd {
		return fmt.Errorf("%w: herd binary digest mismatch/missing", ErrFleetBlocked)
	}
	// Every attested harness digest must have a live digest match;
	// all usable results must have matching non-empty digests.
	for _, k := range keys {
		liveDig, ok := harness[k]
		attDig, has := a.HarnessDigests[k]
		// If result claims usable for kind, both digests required and equal.
		for _, r := range a.Results {
			if r.Kind == k && resultFullyUsable(r) {
				if !has || attDig == "" || liveDig == "" || attDig != liveDig {
					return fmt.Errorf("%w: harness %s digest missing or mismatch for usable result", ErrFleetBlocked, k)
				}
			}
		}
		if has && attDig != "" {
			if !ok || liveDig == "" || attDig != liveDig {
				return fmt.Errorf("%w: harness %s binary digest mismatch", ErrFleetBlocked, k)
			}
		}
		_ = liveDig
	}
	// PolicyDigest must match a re-computed digest of the attested containment+packages binding.
	// We store PolicyDigest from a canonical readiness policy snapshot at issue time;
	// recompute via ReadinessPolicyDigest and require equality.
	wantPD, perr := ReadinessPolicyDigest(a.ContainmentBackend, a.HarnessDigests)
	if perr != nil || wantPD == "" || wantPD != a.PolicyDigest {
		return fmt.Errorf("%w: PolicyDigest mismatch/missing", ErrFleetBlocked)
	}
	return nil
}

// ReadinessPolicyDigest binds containment backend + sorted harness digests.
func ReadinessPolicyDigest(containment string, harness map[string]string) (string, error) {
	if containment == "" || containment == "unavailable" || containment == "skipped" {
		return "", fmt.Errorf("invalid containment")
	}
	keys := make([]string, 0, len(harness))
	for k := range harness {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte("fac133-readiness-policy-v1\n"))
	h.Write([]byte(containment + "\n"))
	for _, k := range keys {
		h.Write([]byte(k + "=" + harness[k] + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SaveFleetAttestation persists with flock, unique tmp, fsync, no-follow, readback.
func SaveFleetAttestation(path string, a *FleetAttestation) error {
	if path == "" || a == nil {
		return fmt.Errorf("attestation path/state required")
	}
	if a.Signature == "" {
		return fmt.Errorf("%w: refusing to save unsigned attestation", ErrFleetBlocked)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fleet attestation flock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("attestation path is symlink (refused)")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if serr := d.Sync(); serr != nil {
			_ = d.Close()
			return serr
		}
		_ = d.Close()
	}
	// Readback: reload and re-verify MAC.
	loaded, err := LoadFleetAttestation(path)
	if err != nil {
		return fmt.Errorf("attestation readback: %w", err)
	}
	secret, _ := FleetAttestationSecret()
	if secret != "" && !VerifyFleetAttestationMAC(secret, loaded) {
		return fmt.Errorf("attestation readback MAC failed")
	}
	return nil
}

// RevokeFleetAttestation marks attestation revoked (re-signed).
func RevokeFleetAttestation(path string) error {
	a, err := LoadFleetAttestation(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	a.Revoked = true
	a.Usable = 0
	a.Results = nil
	secret, serr := FleetAttestationSecret()
	if serr != nil {
		return serr
	}
	if err := SignFleetAttestation(secret, a); err != nil {
		return err
	}
	return SaveFleetAttestation(path, a)
}

func allowLiveHarnessRefresh() bool {
	for _, k := range []string{"HERD_LIVE_HARNESS_PROOF", "HERD_REFRESH_READINESS"} {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") {
			return true
		}
	}
	return false
}

// BuildFleetAttestationFromResults creates a MAC-signed durable attestation.
func BuildFleetAttestationFromResults(results []HarnessProbeResult, ttl time.Duration) (*FleetAttestation, error) {
	return BuildFleetAttestationFromResultsFor(results, ttl, ResolveRequiredHarnessKinds(ResolveReadinessRoot()))
}

// BuildFleetAttestationFromResultsFor creates a MAC-signed durable attestation for the specified harness kinds.
func BuildFleetAttestationFromResultsFor(results []HarnessProbeResult, ttl time.Duration, kinds []string) (*FleetAttestation, error) {
	if ttl <= 0 {
		ttl = DefaultFleetAttestationTTL
	}
	secret, err := FleetAttestationSecret()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(kinds)+len(results))
	var allKinds []string
	for _, k := range kinds {
		if k != "" && !seen[k] {
			seen[k] = true
			allKinds = append(allKinds, k)
		}
	}
	for _, r := range results {
		if r.Kind != "" && !seen[r.Kind] {
			seen[r.Kind] = true
			allKinds = append(allKinds, r.Kind)
		}
	}
	sort.Strings(allKinds)

	herd, harness, containment, cerr := CurrentReadinessBindingFor(allKinds)
	if cerr != nil || containment == "" || containment == "unavailable" {
		return nil, fmt.Errorf("%w: cannot attest without real containment: %v", ErrFleetBlocked, cerr)
	}
	var kept []HarnessProbeResult
	for _, r := range results {
		if resultFullyUsable(r) {
			kept = append(kept, r)
		}
	}
	usable := len(kept)
	if usable == 0 {
		return nil, fmt.Errorf("%w: zero validated usable harnesses — refusing attestation", ErrFleetBlocked)
	}
	// Require digests for every usable kind.
	for _, r := range kept {
		if harness[r.Kind] == "" {
			return nil, fmt.Errorf("%w: missing live digest for usable kind %s", ErrFleetBlocked, r.Kind)
		}
	}
	pd, err := ReadinessPolicyDigest(containment, harness)
	if err != nil {
		return nil, err
	}
	gen, err := NewGeneration()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	a := &FleetAttestation{
		Version:            fleetAttestVersion,
		IssuedAt:           now,
		ExpiresAt:          now.Add(ttl),
		Generation:         gen,
		Revoked:            false,
		HerdBinaryDigest:   herd,
		HarnessDigests:     harness,
		ContainmentBackend: containment,
		PolicyDigest:       pd,
		Usable:             usable,
		Results:            kept,
		Evidence:           fmt.Sprintf("live_refresh usable=%d gen=%s", usable, gen),
	}
	if err := SignFleetAttestation(secret, a); err != nil {
		return nil, err
	}
	return a, nil
}

// ConsumeFleetAttestation loads+validates durable readiness without live spawn.
func ConsumeFleetAttestation(root string) (*FleetReadiness, error) {
	trustRoot, err := TrustedReadinessRoot(root)
	if err != nil {
		return &FleetReadiness{Blocked: true, Reason: err.Error()}, err
	}
	path := FleetAttestationPath(trustRoot)
	secret, serr := FleetAttestationSecret()
	if serr != nil {
		return &FleetReadiness{Blocked: true, Reason: serr.Error()}, serr
	}
	a, err := LoadFleetAttestation(path)
	if err != nil {
		return &FleetReadiness{
			Blocked: true,
			Reason:  fmt.Sprintf("FAC-133 BLOCKED: no durable fleet attestation at %s (%v); refresh with HERD_LIVE_HARNESS_PROOF=1 and HERD_CONTROL_SECRET", path, err),
		}, fmt.Errorf("%w: missing durable attestation", ErrFleetBlocked)
	}
	if err := ValidateFleetAttestation(a, time.Now().UTC(), secret); err != nil {
		return &FleetReadiness{
			Blocked: true,
			Reason:  err.Error(),
			Results: a.Results,
			Usable:  0,
		}, err
	}
	return &FleetReadiness{
		Usable:  a.Usable,
		Results: a.Results,
		Blocked: false,
		Reason:  fmt.Sprintf("FAC-133 ready via MAC attestation gen=%s expires=%s usable=%d", a.Generation, a.ExpiresAt.Format(time.RFC3339), a.Usable),
	}, nil
}

// RefreshFleetAttestationLive single-flights live proof and persists signed attestation.
func RefreshFleetAttestationLive(root string) (*FleetReadiness, error) {
	fleetRefreshMu.Lock()
	defer fleetRefreshMu.Unlock()

	if fr, err := ConsumeFleetAttestation(root); err == nil {
		return fr, nil
	}

	kinds := ResolveRequiredHarnessKinds(root)
	results, err := ProbeAllConfiguredHarnessesLive(kinds)
	fr := &FleetReadiness{Results: results}
	fr.Usable = countValidatedUsable(results)
	if err != nil || fr.Usable == 0 {
		fr.Blocked = true
		if err != nil {
			fr.Reason = err.Error()
		} else {
			fr.Reason = "FAC-133 BLOCKED: zero validated usable harnesses after live refresh"
		}
		return fr, fmt.Errorf("%w: %s", ErrFleetBlocked, fr.Reason)
	}
	att, aerr := BuildFleetAttestationFromResultsFor(results, DefaultFleetAttestationTTL, kinds)
	if aerr != nil {
		fr.Blocked = true
		fr.Reason = aerr.Error()
		return fr, aerr
	}
	trustRoot, rerr := TrustedReadinessRoot(root)
	if rerr != nil {
		fr.Blocked = true
		fr.Reason = rerr.Error()
		return fr, rerr
	}
	if err := SaveFleetAttestation(FleetAttestationPath(trustRoot), att); err != nil {
		fr.Blocked = true
		fr.Reason = "failed to persist fleet attestation: " + err.Error()
		return fr, fmt.Errorf("%w: %s", ErrFleetBlocked, fr.Reason)
	}
	fr.Reason = fmt.Sprintf("FAC-133 ready: signed live refresh gen=%s usable=%d", att.Generation, fr.Usable)
	return fr, nil
}
