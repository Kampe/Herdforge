package feedback

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// replyEnvelope matches bin/herd-mail's on-disk field names exactly ("from",
// "summary") — NOT pkg/mail.Envelope's "sender"/"subject". Reading a real
// fleet reply through pkg/mail's shape would silently miss every line, the
// exact class of interface drift that parked the fleet's census before.
type replyEnvelope struct {
	From    string `json:"from"`
	Summary string `json:"summary"`
}

// ReplyFromLanes reads mailFile (the coordinator's bin/herd-mail-shaped
// inbox) and returns which of the requested lanes replied to epoch and which
// are still missing. A missing file is an empty inbox, not an error. The
// prefix check pins the epoch so a stale reply from a previous census, and
// the census's own request envelope (whose summary has no trailing lane),
// are never counted as a reply.
func ReplyFromLanes(mailFile, epoch string, want []string) (got, missing []string, err error) {
	replied, err := repliedLanes(mailFile, epoch)
	if err != nil {
		return nil, nil, err
	}
	// Replies are evidence only for lanes in this epoch's live expectation.
	// The inbox is append-only and may retain a reply from a lane retired (or
	// rotated) after the request was sent. Returning that stale sender in got
	// made the census print replies greater than its denominator.
	requested := make(map[string]struct{}, len(want))
	for _, lane := range want {
		requested[lane] = struct{}{}
	}
	filtered := make([]string, 0, len(replied))
	for _, lane := range replied {
		if _, ok := requested[lane]; ok {
			filtered = append(filtered, lane)
		}
	}
	return filtered, Missing(want, filtered), nil
}

func repliedLanes(mailFile, epoch string) ([]string, error) {
	if epoch == "" {
		return nil, nil
	}
	data, err := os.ReadFile(mailFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var envs []replyEnvelope
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env replyEnvelope
		// A malformed line fails the whole read (mirrors `jq -sc ... || print
		// '[]'`): a partially parsed reply set could pass through, over-report
		// replies, and mask a genuinely missing lane.
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return nil, nil
		}
		envs = append(envs, env)
	}
	prefix := Subject(epoch) + " "
	seen := map[string]bool{}
	var out []string
	for _, env := range envs {
		if env.From == "" || seen[env.From] || !strings.HasPrefix(env.Summary, prefix) {
			continue
		}
		seen[env.From] = true
		out = append(out, env.From)
	}
	sort.Strings(out)
	return out, nil
}
