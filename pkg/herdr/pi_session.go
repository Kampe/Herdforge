package herdr

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/launch"
)

var (
	ErrPiSessionNotReady      = errors.New("Pi session route is not ready")
	ErrPiSessionRouteMismatch = errors.New("Pi session route mismatch")
	verifyPiSessionRoute      = attestPiSessionRoute
)

func SetPiSessionRouteAttesterForTest(f func(string, []string) error) func() {
	old := verifyPiSessionRoute
	if f == nil {
		verifyPiSessionRoute = attestPiSessionRoute
	} else {
		verifyPiSessionRoute = f
	}
	return func() { verifyPiSessionRoute = old }
}

func expectedPiSessionRoute(argv []string) (string, string, string, error) {
	if (len(argv) != 5 && len(argv) != 7) || argv[0] != "pi" || argv[1] != "--model" || argv[3] != "--thinking" {
		return "", "", "", fmt.Errorf("%w: malformed harness argv", ErrPiSessionRouteMismatch)
	}
	if len(argv) == 7 {
		sessionPath := strings.TrimSpace(argv[6])
		if argv[5] != "--session" || sessionPath == "" || !filepath.IsAbs(sessionPath) {
			return "", "", "", fmt.Errorf("%w: malformed harness argv", ErrPiSessionRouteMismatch)
		}
	}
	provider, model, ok := strings.Cut(strings.TrimSpace(argv[2]), "/")
	thinking := strings.TrimSpace(argv[4])
	if !ok || provider == "" || model == "" || thinking == "" {
		return "", "", "", fmt.Errorf("%w: incomplete harness route", ErrPiSessionRouteMismatch)
	}
	return provider, model, thinking, nil
}

func piSessionRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_SESSION_DIR"))
	if root == "" {
		if configDir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); configDir != "" {
			root = filepath.Join(configDir, "sessions")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("%w: resolve home: %v", ErrPiSessionRouteMismatch, err)
			}
			root = filepath.Join(home, ".pi", "agent", "sessions")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve session root: %v", ErrPiSessionRouteMismatch, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve session root: %v", ErrPiSessionRouteMismatch, err)
	}
	return filepath.Clean(root), nil
}

func openSecurePiSession(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" {
		return nil, fmt.Errorf("%w: session path must be absolute JSONL", ErrPiSessionRouteMismatch)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: session file is not present", ErrPiSessionNotReady)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect session file: %v", ErrPiSessionRouteMismatch, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: session file is not a regular non-symlink file", ErrPiSessionRouteMismatch)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: session file is group/other writable", ErrPiSessionRouteMismatch)
	}
	root, err := piSessionRoot()
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve session file: %v", ErrPiSessionRouteMismatch, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve session file: %v", ErrPiSessionRouteMismatch, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("%w: session file escapes session root", ErrPiSessionRouteMismatch)
	}
	file, err := os.Open(resolved)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: session file disappeared", ErrPiSessionNotReady)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: open session file: %v", ErrPiSessionRouteMismatch, err)
	}
	return file, nil
}

func createPiLaunchSession(req launch.Request) (string, error) {
	if req.Decision == nil || req.Repository == "" || req.TaskRef == "" || req.Lane == "" || req.SessionGeneration <= 0 {
		return "", fmt.Errorf("complete launch identity is required for Pi session allocation")
	}
	root, err := piSessionRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "herdforge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Pi launch session directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("Pi launch session directory is not private")
	}
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", req.Repository, req.TaskRef, req.Lane, req.SessionGeneration, req.Decision.Proof)
	sum := sha256.Sum256([]byte(identity))
	path := filepath.Join(dir, "launch-"+hex.EncodeToString(sum[:])+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create Pi launch session: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("sync Pi launch session: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close Pi launch session: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("open Pi launch session directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		os.Remove(path)
		return "", fmt.Errorf("sync Pi launch session directory: %v %v", syncErr, closeErr)
	}
	return path, nil
}

func attestPiSessionRoute(sessionPath string, harnessArgv []string) error {
	wantProvider, wantModel, wantThinking, err := expectedPiSessionRoute(harnessArgv)
	if err != nil {
		return err
	}
	if len(harnessArgv) == 7 && filepath.Clean(harnessArgv[6]) != filepath.Clean(sessionPath) {
		return fmt.Errorf("%w: bound session path mismatch", ErrPiSessionRouteMismatch)
	}
	file, err := openSecurePiSession(sessionPath)
	if err != nil {
		return err
	}
	defer file.Close()

	type routeEntry struct {
		Type          string `json:"type"`
		Provider      string `json:"provider"`
		ModelID       string `json:"modelId"`
		ThinkingLevel string `json:"thinkingLevel"`
	}
	const maxSessionBytes = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(file, maxSessionBytes+1))
	scanner.Buffer(make([]byte, 4096), maxSessionBytes)
	var gotProvider, gotModel, gotThinking string
	entries := 0
	for scanner.Scan() {
		entries++
		if entries > 128 {
			return fmt.Errorf("%w: route metadata not found within 128 entries", ErrPiSessionRouteMismatch)
		}
		var entry routeEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("%w: malformed session entry: %v", ErrPiSessionRouteMismatch, err)
		}
		if entry.Type == "model_change" && gotProvider == "" && gotModel == "" {
			gotProvider, gotModel = strings.TrimSpace(entry.Provider), strings.TrimSpace(entry.ModelID)
		}
		if entry.Type == "thinking_level_change" && gotThinking == "" {
			gotThinking = strings.TrimSpace(entry.ThinkingLevel)
		}
		if gotProvider != "" && gotModel != "" && gotThinking != "" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: read session metadata: %v", ErrPiSessionRouteMismatch, err)
	}
	if gotProvider == "" || gotModel == "" || gotThinking == "" {
		return fmt.Errorf("%w: model/thinking metadata incomplete", ErrPiSessionNotReady)
	}
	if gotProvider != wantProvider || gotModel != wantModel || gotThinking != wantThinking {
		return fmt.Errorf("%w: got %s/%s thinking=%s want %s/%s thinking=%s", ErrPiSessionRouteMismatch, gotProvider, gotModel, gotThinking, wantProvider, wantModel, wantThinking)
	}
	return nil
}
