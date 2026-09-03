package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	controlMACSecretBytes   = 32
	controlMACSecretMaxSize = 4096
)

var (
	// ErrControlMACSecretRole is returned when a non-coordinator tries to
	// bootstrap or load the control-plane MAC secret for issue/drain.
	ErrControlMACSecretRole = fmt.Errorf("%w: control MAC secret bootstrap/load is coordinator-only", ErrUnknownPolicy)
	// ErrControlMACSecretConflict is a fail-closed mismatch between the
	// explicit env secret and the durable file, or a mutation attempt.
	ErrControlMACSecretConflict = fmt.Errorf("%w: control MAC secret conflict", ErrUnknownPolicy)
	// ErrControlMACSecretUnusable covers symlink, broad mode, empty, or
	// corrupt durable material. The last valid file is never rewritten.
	ErrControlMACSecretUnusable = fmt.Errorf("%w: control MAC secret unusable", ErrUnknownPolicy)
)

// ControlMACSecretPath is the coordinator-only secret used by wrapper re-verify.
func ControlMACSecretPath(sharedRoot string) string {
	return filepath.Join(sharedRoot, ".herd", "control", "mac.secret")
}

// BootstrapOrLoadControlMACSecret is the coordinator-only first-use
// create-or-read path. envSecret is optional; when set it must match the
// durable file or fail closed. Non-coordinator roles never create or receive
// the secret. The returned secret is for in-process MAC use only — callers
// must not print, log, or place it on argv.
func BootstrapOrLoadControlMACSecret(sharedRoot, envSecret, role string) (string, bool, error) {
	if strings.ToLower(strings.TrimSpace(role)) != "coordinator" {
		return "", false, fmt.Errorf("%w", ErrControlMACSecretRole)
	}
	if strings.TrimSpace(sharedRoot) == "" {
		return "", false, fmt.Errorf("%w: shared root required", ErrUnknownPolicy)
	}
	envSecret = strings.TrimSpace(envSecret)
	if envSecret != "" {
		if err := validateControlMACSecretMaterial(envSecret); err != nil {
			return "", false, err
		}
	}
	var (
		out     string
		created bool
	)
	err := withControlMACSecretLock(sharedRoot, func() error {
		existing, err := inspectControlMACSecret(sharedRoot)
		if err == nil {
			if envSecret != "" && envSecret != existing {
				return fmt.Errorf("%w: explicit env secret does not match durable mac.secret", ErrControlMACSecretConflict)
			}
			out = existing
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		secret := envSecret
		if secret == "" {
			secret, err = generateControlMACSecret()
			if err != nil {
				return err
			}
		}
		if err := writeControlMACSecretLocked(sharedRoot, secret); err != nil {
			return err
		}
		out = secret
		created = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return out, created, nil
}

// WriteControlMACSecret stores the MAC secret under shared root with flock,
// unique tmp, O_EXCL, no-follow replace, file+dir fsync, and readback.
// An existing valid secret is preserved: equal writes are idempotent, and a
// different secret is refused.
func WriteControlMACSecret(sharedRoot, secret string) error {
	if strings.TrimSpace(sharedRoot) == "" {
		return fmt.Errorf("%w: shared root and secret required", ErrUnknownPolicy)
	}
	secret = strings.TrimSpace(secret)
	if err := validateControlMACSecretMaterial(secret); err != nil {
		return err
	}
	return withControlMACSecretLock(sharedRoot, func() error {
		existing, err := inspectControlMACSecret(sharedRoot)
		if err == nil {
			if existing == secret {
				return nil
			}
			return fmt.Errorf("%w: durable mac.secret already exists", ErrControlMACSecretConflict)
		}
		if !os.IsNotExist(err) {
			return err
		}
		return writeControlMACSecretLocked(sharedRoot, secret)
	})
}

// ReadControlMACSecret loads the coordinator MAC secret from shared root.
// Symlink, broad permissions, and empty/corrupt content fail closed.
func ReadControlMACSecret(sharedRoot string) (string, error) {
	if strings.TrimSpace(sharedRoot) == "" {
		return "", fmt.Errorf("%w: shared root required", ErrUnknownPolicy)
	}
	var out string
	err := withControlMACSecretLock(sharedRoot, func() error {
		secret, err := inspectControlMACSecret(sharedRoot)
		if err != nil {
			return err
		}
		out = secret
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func generateControlMACSecret() (string, error) {
	var raw [controlMACSecretBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate control MAC secret: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateControlMACSecretMaterial(secret string) error {
	if secret == "" || strings.TrimSpace(secret) == "" {
		return fmt.Errorf("%w: empty secret", ErrControlMACSecretUnusable)
	}
	if strings.IndexByte(secret, 0) >= 0 {
		return fmt.Errorf("%w: NUL in secret", ErrControlMACSecretUnusable)
	}
	if len(secret) > controlMACSecretMaxSize {
		return fmt.Errorf("%w: secret too large", ErrControlMACSecretUnusable)
	}
	return nil
}

func withControlMACSecretLock(sharedRoot string, fn func() error) error {
	path := ControlMACSecretPath(sharedRoot)
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
			return fmt.Errorf("mac.secret flock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func inspectControlMACSecret(sharedRoot string) (string, error) {
	path := ControlMACSecretPath(sharedRoot)
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: mac.secret path is symlink", ErrControlMACSecretUnusable)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: mac.secret is not a regular file", ErrControlMACSecretUnusable)
	}
	if fi.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("%w: mac.secret mode must be 0600", ErrControlMACSecretUnusable)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("%w: open mac.secret: %v", ErrControlMACSecretUnusable, err)
	}
	defer f.Close()
	raw := make([]byte, controlMACSecretMaxSize+1)
	n, err := f.Read(raw)
	if err != nil && n == 0 {
		return "", fmt.Errorf("%w: read mac.secret: %v", ErrControlMACSecretUnusable, err)
	}
	if n > controlMACSecretMaxSize {
		return "", fmt.Errorf("%w: secret too large", ErrControlMACSecretUnusable)
	}
	secret := strings.TrimSpace(string(raw[:n]))
	if err := validateControlMACSecretMaterial(secret); err != nil {
		return "", err
	}
	return secret, nil
}

func writeControlMACSecretLocked(sharedRoot, secret string) error {
	path := ControlMACSecretPath(sharedRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(secret)); err != nil {
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
		return fmt.Errorf("%w: mac.secret path is symlink", ErrControlMACSecretUnusable)
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
	got, err := inspectControlMACSecret(sharedRoot)
	if err != nil {
		return fmt.Errorf("mac.secret readback: %w", err)
	}
	if got != secret {
		return fmt.Errorf("mac.secret readback mismatch")
	}
	return nil
}
