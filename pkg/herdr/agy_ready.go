package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const agyPinnedModelReadyBudget = 30 * time.Second

var (
	agyCLILogName   = regexp.MustCompile(`^cli-(\d{8}_\d{6})\.log$`)
	agyCLILogStamp  = "20060102_150405"
	agyConversation = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type agyLogCand struct {
	path  string
	stamp time.Time
}

// AgyPinnedModelReadyRequest binds one AGY TUI launch. Callers must pass the
// launch's own cwd, start time, and pinned model. Global log greps without
// that binding are refused.
type AgyPinnedModelReadyRequest struct {
	Home          string
	Cwd           string
	PinnedModel   string
	StartedAt     time.Time
	PID           int
	Budget        time.Duration
	LogDir        string
	IndexPath     string
	Conversations string
}

// AgyPinnedModelReadyEvidence is launch-specific proof that the pinned model
// overrode after this process authenticated. ConversationID is AGY's UUID.
type AgyPinnedModelReadyEvidence struct {
	LogFile        string
	PinnedModel    string
	ConversationID string
}

// AwaitAgyPinnedModelReady waits until THIS launch's AGY TUI has applied the
// exact pinned model after auth. It does not drop or replace the pin, does not
// mutate auth, and does not treat interactive_ready as sufficient.
func AwaitAgyPinnedModelReady(req AgyPinnedModelReadyRequest) (AgyPinnedModelReadyEvidence, error) {
	var zero AgyPinnedModelReadyEvidence
	req.Home = strings.TrimSpace(req.Home)
	req.Cwd = strings.TrimSpace(req.Cwd)
	req.PinnedModel = strings.TrimSpace(req.PinnedModel)
	if req.Home == "" || req.Cwd == "" || req.PinnedModel == "" || req.StartedAt.IsZero() {
		return zero, fmt.Errorf("agy pinned-model readiness requires home, cwd, pinned model, and launch start time")
	}
	budget := req.Budget
	if budget <= 0 {
		budget = agyPinnedModelReadyBudget
	}
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		ev, err := inspectAgyPinnedModelReady(req)
		if err == nil {
			return ev, nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("agy pinned model %s not ready for this launch within %s", req.PinnedModel, budget)
			}
			return zero, lastErr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func inspectAgyPinnedModelReady(req AgyPinnedModelReadyRequest) (AgyPinnedModelReadyEvidence, error) {
	var zero AgyPinnedModelReadyEvidence
	logFile, err := launchAgyLogFile(req)
	if err != nil {
		return zero, err
	}
	body, err := os.ReadFile(logFile)
	if err != nil {
		return zero, fmt.Errorf("agy launch log: %w", err)
	}
	if req.PID > 0 && !agyLogBoundToPID(string(body), req.PID) {
		return zero, fmt.Errorf("agy launch log %s is not bound to pid %d", filepath.Base(logFile), req.PID)
	}
	if !agyLogPinnedModelReady(string(body), req.PinnedModel) {
		return zero, fmt.Errorf("agy launch log %s has not applied pinned model %s after auth", filepath.Base(logFile), req.PinnedModel)
	}
	ev := AgyPinnedModelReadyEvidence{LogFile: logFile, PinnedModel: req.PinnedModel}
	if id, err := agyLaunchConversationID(req); err == nil {
		ev.ConversationID = id
	}
	return ev, nil
}

func agyLogPinnedModelReady(body, pinned string) bool {
	pinned = strings.TrimSpace(pinned)
	if pinned == "" {
		return false
	}
	resolving := "Resolving model " + pinned
	failed := "failed to apply model override"
	propagate := "Propagating selected model override"
	lastFail := strings.LastIndex(body, failed)
	lastResolve := strings.LastIndex(body, resolving)
	lastProp := strings.LastIndex(body, propagate)
	if lastResolve < 0 || lastProp < 0 {
		return false
	}
	if lastFail > lastProp {
		return false
	}
	return lastProp > lastResolve || (lastResolve >= 0 && lastProp >= 0 && lastFail < lastProp)
}

func launchAgyLogFile(req AgyPinnedModelReadyRequest) (string, error) {
	dir := strings.TrimSpace(req.LogDir)
	if dir == "" {
		dir = filepath.Join(req.Home, ".gemini", "antigravity-cli", "log")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("agy launch log dir: %w", err)
	}
	floor := req.StartedAt.Add(-2 * time.Second)
	var matched []agyLogCand
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m := agyCLILogName.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		stamp, err := time.ParseInLocation(agyCLILogStamp, m[1], time.Local)
		if err != nil {
			continue
		}
		if stamp.Before(floor) {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		if req.PID > 0 {
			body, readErr := os.ReadFile(path)
			if readErr != nil || !agyLogBoundToPID(string(body), req.PID) {
				continue
			}
		}
		matched = append(matched, agyLogCand{path: path, stamp: stamp})
	}
	if req.PID > 0 && len(matched) == 0 {
		return "", fmt.Errorf("no agy launch log bound to pid %d after %s", req.PID, req.StartedAt.Format(time.RFC3339Nano))
	}
	picked := pickAgyLaunchLog(matched, req.StartedAt)
	if picked == "" {
		return "", fmt.Errorf("no agy launch log bound to start %s", req.StartedAt.Format(time.RFC3339Nano))
	}
	return picked, nil
}

func agyLogBoundToPID(body string, pid int) bool {
	if pid <= 0 {
		return false
	}
	re := regexp.MustCompile(`(?m)(?:^|[^\d])pid ` + regexp.QuoteMeta(strconv.Itoa(pid)) + `(?:[^\d]|$)`)
	return re.MatchString(body)
}

func pickAgyLaunchLog(cands []agyLogCand, started time.Time) string {
	if len(cands) == 0 {
		return ""
	}
	var after []agyLogCand
	for _, c := range cands {
		if !c.stamp.Before(started.Truncate(time.Second)) {
			after = append(after, c)
		}
	}
	pool := after
	if len(pool) == 0 {
		return ""
	}
	best := pool[0]
	same := 1
	for _, c := range pool[1:] {
		if c.stamp.Before(best.stamp) {
			best = c
			same = 1
			continue
		}
		if c.stamp.Equal(best.stamp) && c.path != best.path {
			same++
		}
	}
	if same > 1 {
		return ""
	}
	return best.path
}

func agyLaunchConversationID(req AgyPinnedModelReadyRequest) (string, error) {
	indexPath := strings.TrimSpace(req.IndexPath)
	if indexPath == "" {
		indexPath = filepath.Join(req.Home, ".gemini", "antigravity-cli", "cache", "last_conversations.json")
	}
	body, err := os.ReadFile(indexPath)
	if err != nil {
		return "", fmt.Errorf("agy conversation index: %w", err)
	}
	var index map[string]string
	if err := json.Unmarshal(body, &index); err != nil || index == nil {
		return "", fmt.Errorf("agy conversation index is invalid")
	}
	want := filepath.Clean(req.Cwd)
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	id := ""
	for recorded, value := range index {
		got := filepath.Clean(recorded)
		if resolved, err := filepath.EvalSymlinks(got); err == nil {
			got = resolved
		}
		if got != want {
			continue
		}
		value = strings.TrimSpace(value)
		if !agyConversation.MatchString(value) || !RealModelSessionID(value) {
			return "", fmt.Errorf("agy conversation id is not a genuine UUID")
		}
		if id != "" && id != value {
			return "", fmt.Errorf("agy conversation mapping is ambiguous for this cwd")
		}
		id = value
	}
	if id == "" {
		return "", fmt.Errorf("agy conversation UUID for this cwd is not recorded yet")
	}
	convDir := strings.TrimSpace(req.Conversations)
	if convDir == "" {
		convDir = filepath.Join(req.Home, ".gemini", "antigravity-cli", "conversations")
	}
	db := filepath.Join(convDir, id+".db")
	info, err := os.Stat(db)
	if err != nil {
		return "", fmt.Errorf("agy conversation db for %s: %w", id, err)
	}
	if info.ModTime().Before(req.StartedAt) {
		return "", fmt.Errorf("agy conversation %s predates this launch", id)
	}
	return id, nil
}
