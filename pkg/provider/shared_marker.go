package provider

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	sharedMarkerLeaf  = "SHARED"
	sharedMarkerMagic = "herd-shared-fence-v5"
	fencesDBLeaf      = "fences.db"
	envClaimDir       = "HERD_CLAIM_DIR"
	envVolumeID       = "HERD_FENCE_VOLUME_ID"
	envProvision      = "HERD_FENCE_PROVISION"
	envRotate         = "HERD_FENCE_ROTATE"
	envProvisionToken = "HERD_FENCE_PROVISION_TOKEN"
)

// WriteSharedMarker provisions the shared fence authority once:
//  1. Creates fences.db with an internal store_authority.volume_seal row
//     (trustworthy: seal lives only inside the atomic SQLite store).
//  2. Writes SHARED human-readable pointer (not the source of truth).
// ValidateSharedMarker reads the seal FROM THE DB, not from forgeable fields.
func WriteSharedMarker(dir string) error {
	if os.Getenv(envProvision) != "1" && os.Getenv(envRotate) != "1" {
		return fmt.Errorf("provider: WriteSharedMarker requires %s=1 or %s=1; use herd fence-provision", envProvision, envRotate)
	}
	claimDir := strings.TrimSpace(os.Getenv(envClaimDir))
	if claimDir == "" {
		return fmt.Errorf("provider: WriteSharedMarker requires %s (no repo-local fallback)", envClaimDir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	want, err := filepath.Abs(claimDir)
	if err != nil {
		return err
	}
	if abs != want {
		return fmt.Errorf("provider: provision dir %q != %s %q", abs, envClaimDir, want)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	fencePath := filepath.Join(abs, fencesDBLeaf)

	// Existing seal?
	if existing, err := readDBVolumeSeal(fencePath); err == nil && existing != "" {
		if os.Getenv(envRotate) != "1" {
			return fmt.Errorf("provider: store already sealed (volume_seal present); refuse overwrite — %s=1 with matching %s", envRotate, envVolumeID)
		}
		curEnv := strings.TrimSpace(os.Getenv(envVolumeID))
		if curEnv == "" {
			curEnv = strings.TrimSpace(os.Getenv(envProvisionToken))
		}
		if curEnv == "" || curEnv != existing {
			return fmt.Errorf("provider: rotate requires %s matching existing DB volume_seal", envVolumeID)
		}
		newSeal, err := mintSeal()
		if err != nil {
			return err
		}
		if err := writeDBVolumeSeal(fencePath, newSeal, true); err != nil {
			return err
		}
		return writeSHAREDPointer(abs, want, newSeal, existing)
	} else if err != nil && !os.IsNotExist(err) && !isNoSealErr(err) {
		return err
	}

	if os.Getenv(envProvision) != "1" {
		return fmt.Errorf("provider: no sealed fence store; set %s=1 for first-time provision", envProvision)
	}
	// First-time provision ALWAYS mints a fresh random seal. Caller-supplied
	// HERD_FENCE_VOLUME_ID / PROVISION_TOKEN must not seed a new store — that
	// allowed independent hosts to plant a stolen seal and pass validation
	// (audit con62fkm #4 self-mintable split-brain). Join/validate uses env
	// match against the already-provisioned DB row only.
	if envSeal := strings.TrimSpace(os.Getenv(envVolumeID)); envSeal != "" {
		return fmt.Errorf("provider: first-time provision refuses pre-set %s (would enable stolen-seal split-brain); unset it, provision once, then distribute the minted seal", envVolumeID)
	}
	if envTok := strings.TrimSpace(os.Getenv(envProvisionToken)); envTok != "" {
		return fmt.Errorf("provider: first-time provision refuses pre-set %s; unset it, provision once, then distribute the minted seal", envProvisionToken)
	}
	seal, err := mintSeal()
	if err != nil {
		return err
	}
	if err := writeDBVolumeSeal(fencePath, seal, false); err != nil {
		return err
	}
	return writeSHAREDPointer(abs, want, seal, "")
}

func mintSeal() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isNoSealErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no volume_seal")
}

func writeDBVolumeSeal(fencePath, seal string, rotate bool) error {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", fencePath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	// Authority is volume_seal + exclusive flock on claim volume (non-copyable
	// live authority). Absolute claim_path is NOT stored (copyable/split-brain
	// and violates generated-artifact absolute-path invariant).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS store_authority (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		volume_seal TEXT NOT NULL UNIQUE,
		updated_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if rotate {
		if _, err := db.Exec(`UPDATE store_authority SET volume_seal = ?, updated_at = datetime('now') WHERE id = 1`, seal); err != nil {
			return err
		}
	} else {
		// INSERT only — refuse if row exists (one-time).
		res, err := db.Exec(`INSERT INTO store_authority (id, volume_seal, updated_at) VALUES (1, ?, datetime('now'))`, seal)
		if err != nil {
			return fmt.Errorf("provider: seal insert failed (already provisioned?): %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("provider: seal insert rows=%d", n)
		}
	}
	// Also ensure fence high-water table exists so OpenClaimStack can reopen.
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS fences (
		task_id TEXT PRIMARY KEY NOT NULL,
		fence_high INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL
	)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS applied_ops (
		op_id TEXT PRIMARY KEY NOT NULL,
		task_id TEXT NOT NULL,
		fence_token INTEGER NOT NULL,
		revision TEXT NOT NULL DEFAULT '',
		expected_status TEXT NOT NULL DEFAULT '',
		expected_comment TEXT NOT NULL DEFAULT '',
		ambiguous INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME NOT NULL
	)`)
	return nil
}

// ReadFenceVolumeSeal returns store_authority.volume_seal from claimDir/fences.db.
func ReadFenceVolumeSeal(claimDir string) (string, error) {
	abs, err := filepath.Abs(claimDir)
	if err != nil {
		return "", err
	}
	return readDBVolumeSeal(filepath.Join(abs, fencesDBLeaf))
}

func readDBVolumeSeal(fencePath string) (string, error) {
	if _, err := os.Stat(fencePath); err != nil {
		return "", err
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", fencePath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var seal string
	err = db.QueryRow(`SELECT volume_seal FROM store_authority WHERE id = 1`).Scan(&seal)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no volume_seal in fence store")
	}
	if err != nil {
		// Table missing
		if strings.Contains(err.Error(), "no such table") {
			return "", fmt.Errorf("no volume_seal in fence store")
		}
		return "", err
	}
	return seal, nil
}

func writeSHAREDPointer(abs, claimDir, seal, rotatedFrom string) error {
	// Do NOT write the volume seal into SHARED: the file is mode 0644 and must
	// not carry HMAC/identity secrets. Authority is fences.db store_authority
	// row matched against HERD_FENCE_VOLUME_ID from the process environment.
	_ = seal
	lines := []string{
		sharedMarkerMagic,
		"version=5",
		"claim_dir=" + claimDir,
		"authority=store_authority.volume_seal",
		"note=seal lives only in fences.db store_authority; HERD_FENCE_VOLUME_ID is env-only (never written here)",
	}
	if rotatedFrom != "" {
		// rotated_from is a prior seal id — also secret; record only that rotation happened.
		lines = append(lines, "rotated=1")
	}
	lines = append(lines, "")
	path := filepath.Join(abs, sharedMarkerLeaf)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	df, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer df.Close()
	return df.Sync()
}

// ValidateSharedMarker reads volume_seal from fences.db (authoritative).
// A copied volume_id in SHARED alone cannot pass without the sealed DB row.
func ValidateSharedMarker(dir string) error {
	if dir == "" {
		return fmt.Errorf("provider: empty claim dir")
	}
	claimDir := strings.TrimSpace(os.Getenv(envClaimDir))
	if claimDir == "" {
		return fmt.Errorf("provider: %s required", envClaimDir)
	}
	wantVol := strings.TrimSpace(os.Getenv(envVolumeID))
	if wantVol == "" {
		wantVol = strings.TrimSpace(os.Getenv(envProvisionToken))
	}
	if wantVol == "" || len(wantVol) < 32 {
		return fmt.Errorf("provider: %s required (min 32 chars)", envVolumeID)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	want, err := filepath.Abs(claimDir)
	if err != nil {
		return err
	}
	if abs != want {
		return fmt.Errorf("provider: claim dir %q != %s %q", abs, envClaimDir, want)
	}
	fencePath := filepath.Join(abs, fencesDBLeaf)
	seal, err := readDBVolumeSeal(fencePath)
	if err != nil {
		return fmt.Errorf("provider: shared fence store seal: %w (run herd fence-provision on the shared volume)", err)
	}
	if seal != wantVol {
		return fmt.Errorf("provider: fences.db volume_seal mismatch (not this fleet store; independent/forged store refused)")
	}
	return nil
}

// ReadVolumeSeal returns the durable volume_seal from fences.db under claimDir.
// Used by `herd fence-provision` to print the mint for fleet distribution.
func ReadVolumeSeal(claimDir string) (string, error) {
	if claimDir == "" {
		return "", fmt.Errorf("provider: empty claim dir")
	}
	abs, err := filepath.Abs(claimDir)
	if err != nil {
		return "", err
	}
	return readDBVolumeSeal(filepath.Join(abs, fencesDBLeaf))
}

// ProvisionSharedFenceForTest provisions sealed fence store + env for tests.
func ProvisionSharedFenceForTest(t interface {
	Helper()
	Setenv(key, value string)
	Fatal(...any)
}, dir string) {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProvision, "1")
	t.Setenv(envClaimDir, abs)
	t.Setenv(envVolumeID, "")
	t.Setenv(envProvisionToken, "")
	t.Setenv(envRotate, "")
	// HMAC key for status receipts in tests.
	t.Setenv(envFenceHMACKey, "test-hmac-key-fac147-min-16b")
	if err := WriteSharedMarker(abs); err != nil {
		t.Fatal(err)
	}
	seal, err := readDBVolumeSeal(filepath.Join(abs, fencesDBLeaf))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVolumeID, seal)
	t.Setenv(envProvisionToken, seal)
	t.Setenv(envProvision, "")
}
