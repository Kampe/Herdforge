package router

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
)

// AgyPrintEnvelope is the documented `agy --output-format json` payload.
// Slash-command print answers use an empty conversation_id; a model probe
// must be a real assistant turn. Stream-json NDJSON is not accepted: the
// probe requests json and requires exactly one complete document.
type AgyPrintEnvelope struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Model          string `json:"model"`
}

// InsertAgyStructuredPrintFlags puts --output-format json and
// --disable-slash-commands before --print/-p/--prompt.
//
// --print consumes the next argv as the prompt, so flags after it become the
// prompt and never reach print-mode JSON. FAC-855 admission and herdr model
// probes share this helper.
func InsertAgyStructuredPrintFlags(argv []string) []string {
	out := make([]string, 0, len(argv)+4)
	inserted := false
	for _, a := range argv {
		if !inserted && (a == "--print" || a == "-p" || a == "--prompt") {
			out = append(out, OutputFormatFlag, "json", agentpolicy.DisableSlashCommandsFlag)
			inserted = true
		}
		out = append(out, a)
	}
	if !inserted {
		out = append(out, OutputFormatFlag, "json", agentpolicy.DisableSlashCommandsFlag)
	}
	return out
}

// DecodeAgyPrintEnvelope decodes exactly one JSON value and refuses trailing
// content or a second result.
func DecodeAgyPrintEnvelope(stdout string) (AgyPrintEnvelope, error) {
	s := strings.TrimSpace(stdout)
	dec := json.NewDecoder(strings.NewReader(s))
	var env AgyPrintEnvelope
	if err := dec.Decode(&env); err != nil {
		return AgyPrintEnvelope{}, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return AgyPrintEnvelope{}, fmt.Errorf("trailing content after agy json envelope")
	}
	if env.Status == "" && env.Response == "" && env.ConversationID == "" {
		return AgyPrintEnvelope{}, fmt.Errorf("agy print envelope not found")
	}
	return env, nil
}

// AgyStructuredProbeReason reports why an AGY structured-print payload is not
// a successful probe. An empty string means SUCCESS, a conversation session,
// and a response exactly equal to expectedToken. requestedModel is checked
// only when the envelope names a model.
func AgyStructuredProbeReason(stdout, requestedModel, expectedToken string) string {
	env, err := DecodeAgyPrintEnvelope(stdout)
	if err != nil {
		return "no structured agy probe result"
	}
	if !strings.EqualFold(strings.TrimSpace(env.Status), "SUCCESS") {
		return "agy probe status is not SUCCESS"
	}
	if strings.TrimSpace(env.ConversationID) == "" {
		return "agy probe missing conversation session"
	}
	if strings.TrimSpace(env.Response) != expectedToken {
		return "agy probe response is not the exact token"
	}
	if got := strings.TrimSpace(env.Model); got != "" && got != strings.TrimSpace(requestedModel) {
		return "agy probe model does not match requested model"
	}
	return ""
}
