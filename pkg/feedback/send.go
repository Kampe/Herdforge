package feedback

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// mailEnvelope mirrors bin/herd-mail's on-disk shape exactly: "from" and
// "summary", not pkg/mail.Envelope's "sender"/"subject". Any reader of this
// file, including this package's own reply census, keys off these names.
type mailEnvelope struct {
	ID        int64   `json:"id"`
	Type      string  `json:"type"`
	From      string  `json:"from"`
	To        string  `json:"to"`
	Timestamp string  `json:"timestamp"`
	Summary   string  `json:"summary"`
	Message   string  `json:"message"`
	Category  string  `json:"category"`
	ReadAt    *string `json:"read_at"`
	AckAt     *string `json:"ack_at"`
}

// DefaultDurableMail appends a message envelope to mailDir/<to>.jsonl in
// bin/herd-mail's exact wire shape, from the coordinator, under an advisory
// per-file lock.
//
// ponytail: a single flock, not chainseer's full creator-lease/quarantine
// recovery protocol — this coordinator process is the only writer to a
// lane's inbox during a census. Adopt the fuller protocol if a second
// concurrent writer to the same inbox ever needs to be made safe here.
func DefaultDurableMail(mailDir, coordinator string) func(context.Context, string, string, string) error {
	return DurableMail(mailDir, coordinator)
}

// DurableMail returns an append-only sender for the herd-mail wire shape.
// The sender is explicit because a feedback reply is written by a lane, while
// the census request is written by the coordinator.
func DurableMail(mailDir, sender string) func(context.Context, string, string, string) error {
	return func(_ context.Context, to, summary, body string) error {
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("recipient lane is required")
		}
		if err := os.MkdirAll(mailDir, 0o700); err != nil {
			return fmt.Errorf("create mail dir: %w", err)
		}
		path := filepath.Join(mailDir, to+".jsonl")
		unlock, err := lockMailFile(path)
		if err != nil {
			return fmt.Errorf("lock inbox %s: %w", path, err)
		}
		defer unlock()
		id, err := nextMailID(path)
		if err != nil {
			return fmt.Errorf("read inbox %s: %w", path, err)
		}
		env := mailEnvelope{
			ID: id, Type: "message", From: sender, To: to,
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			Summary:   summary, Message: body, Category: "informational",
		}
		line, err := json.Marshal(env)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
		return f.Sync()
	}
}

// RecordReply records the documented `herd send <coordinator>
// "FLEET_FEEDBACK <epoch> <lane>" <body>` reply in the coordinator's durable
// inbox. It returns nil for ordinary herd-send text so callers can use this as
// a narrowly-scoped compatibility bridge without changing normal sends.
func RecordReply(ctx context.Context, mailDir, lane, coordinator, text string) error {
	fields := strings.Fields(text)
	if len(fields) == 0 || fields[0] != SubjectPrefix {
		return nil
	}
	if len(fields) < 3 {
		return fmt.Errorf("feedback reply requires epoch and lane")
	}
	if fields[1] == "" || fields[2] == "" {
		return fmt.Errorf("feedback reply requires epoch and lane")
	}
	if strings.TrimSpace(lane) == "" || strings.TrimSpace(coordinator) == "" {
		return fmt.Errorf("feedback reply requires lane and coordinator")
	}
	if fields[2] != lane {
		return fmt.Errorf("feedback reply lane %q does not match HERD_LANE %q", fields[2], lane)
	}
	summary := strings.Join(fields[:3], " ")
	body := strings.TrimSpace(strings.TrimPrefix(text, summary))
	return DurableMail(mailDir, lane)(ctx, coordinator, summary, body)
}

// DefaultMailDir resolves the same repo-scoped default used by the census.
func DefaultMailDir(repoRoot string) string {
	if configured := strings.TrimSpace(os.Getenv(EnvMailDir)); configured != "" {
		return configured
	}
	return filepath.Join(defaultFleetStateDir(repoRoot), "mail")
}

func nextMailID(path string) (int64, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var max int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal([]byte(line), &env) == nil && env.ID > max {
			max = env.ID
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return max + 1, nil
}

func lockMailFile(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// DefaultWake nudges a settled lane. When sendBin is set (HERD_SEND_BIN) it
// execs that binary as `sendBin <lane> <text>`; otherwise it uses herdr's
// own verified pane-prompt delivery — the Go port of bin/herd-send already
// live in this repo — so a working default never depends on an external
// script this repo does not ship.
func DefaultWake(sendBin string) func(context.Context, string, string) error {
	return func(ctx context.Context, lane, nudge string) error {
		if strings.TrimSpace(sendBin) != "" {
			cmd := exec.CommandContext(ctx, sendBin, lane, nudge)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %s: %w", sendBin, strings.TrimSpace(string(out)), err)
			}
			return nil
		}
		_, err := herdr.Send(lane, nudge, true, 30*time.Second)
		return err
	}
}
