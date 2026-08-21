package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/Kampe/Herdforge/pkg/textdelivery"
)

// OperatorDelivery is one compiled, durable Herdr delivery. Payload is a
// structured source (bytes or file) so free-form text never arrives as a
// shell-interpolated string. The transport places exact payload bytes in
// herdr's TEXT argv element via direct process argv (never zsh -c / eval).
//
// Key + Generation bind the durable receipt. Target is the pane/name herdr
// accepts. Session is optional provenance for identity binding; empty is
// allowed because some agent kinds (grok) report no session id.
type OperatorDelivery struct {
	Key        string
	Generation int64
	Target     string
	Session    string
	Wait       bool
	Payload    textdelivery.Payload
	StatePath  string
	Timeout    time.Duration
}

// DeliveryProof is the durable, independently checkable receipt for one
// operator delivery.
type DeliveryProof struct {
	Key            string   `json:"key"`
	Generation     int64    `json:"generation"`
	Target         string   `json:"target"`
	Session        string   `json:"session,omitempty"`
	Wait           bool     `json:"wait"`
	Executable     string   `json:"executable"`
	Argv           []string `json:"argv"`
	PayloadSHA256  string   `json:"payload_sha256"`
	IntentSHA256   string   `json:"intent_sha256"`
	ReadbackSHA256 string   `json:"readback_sha256"`
	Readback       []byte   `json:"readback"`
	Verified       bool     `json:"verified"`
	Consumed       bool     `json:"consumed"`
	BaselineStatus string   `json:"baseline_status"`
	FinalStatus    string   `json:"final_status"`
	SequenceToken  string   `json:"sequence_token"`
	SawWorking     bool     `json:"saw_working"`
}

type operatorReadback struct {
	Version        int    `json:"version"`
	Key            string `json:"key"`
	Target         string `json:"target"`
	Session        string `json:"session,omitempty"`
	BaselineStatus string `json:"baseline_status"`
	FinalStatus    string `json:"final_status"`
	SequenceToken  string `json:"sequence_token"`
	Submitted      bool   `json:"submitted"`
	Consumed       bool   `json:"consumed"`
	Verified       bool   `json:"verified"`
	SawWorking     bool   `json:"saw_working"`
	PayloadSHA256  string `json:"payload_sha256"`
}

// statusProbe is injectable for hermetic tests. Production uses liveStatus.
var statusProbe = liveStatus

func canonicalHerdrIdentity(target, session string) []byte {
	return []byte(fmt.Sprintf("target:%d:%s;session:%d:%s", len(target), target, len(session), session))
}

func operatorReadbackPolicy(key, target, session string) textdelivery.ReadbackPolicy {
	return func(payload, readback []byte) bool {
		var proof operatorReadback
		if json.Unmarshal(readback, &proof) != nil || proof.Version != 1 {
			return false
		}
		if proof.Key != key || proof.Target != target || proof.Session != session {
			return false
		}
		// Baseline may be empty: a freshly launched agent is often not yet in
		// inventory (same as DeliverAndProve). Empty is a valid pre-submit
		// baseline; ConsumptionProvenSeen decides whether the transition is proof.
		if !proof.Submitted || !proof.Consumed || !proof.Verified {
			return false
		}
		if !ConsumptionProvenSeen(proof.BaselineStatus, proof.FinalStatus, proof.SawWorking) {
			return false
		}
		if proof.SequenceToken != sequenceToken(proof.BaselineStatus, proof.FinalStatus) {
			return false
		}
		return proof.PayloadSHA256 == textdelivery.Digest(payload)
	}
}

// waitForOperatorConsumption polls agent status until ConsumptionProvenSeen
// holds, or the deadline / context expires. Uses the same proof rules as
// DeliverAndProve (including sawWorking for bare idle→done rejection).
//
// When waitCLI is true, the herdr CLI was already asked for --wait --until
// working; we still poll for sawWorking evidence and sequence tokens so the
// durable receipt matches DeliverAndProve semantics.
func waitForOperatorConsumption(ctx context.Context, key, target, session, baseline, payloadSHA string, timeout time.Duration) (operatorReadback, error) {
	return waitForOperatorConsumptionText(ctx, key, target, session, baseline, payloadSHA, "", timeout)
}

// waitForOperatorConsumptionText additionally accepts the payload text so
// consumption can be proven by OBSERVING it in the pane, not only by catching a
// transient status transition.
//
// FAC-545: proving consumption from status alone is racy. A lane that is idle
// ("done"), consumes the assignment quickly, and returns to "done" can pass
// through "working" entirely between polls — the loop samples every 2s for a
// long timeout — so sawWorking never becomes true and a genuinely consumed
// assignment is reported queued-but-not-consumed. Chainseer demonstrated this
// deterministically on an idle lane whose pane visibly contained both the
// assignment and the agent's exact reply.
func waitForOperatorConsumptionText(ctx context.Context, key, target, session, baseline, payloadSHA, payloadText string, timeout time.Duration) (operatorReadback, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	poll := 200 * time.Millisecond
	if timeout > 5*time.Second {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	nudged := false
	last := baseline
	sawWorking := baseline == "working"
	for {
		if err := ctx.Err(); err != nil {
			return operatorReadback{}, err
		}
		st, err := statusProbe(target)
		if err == nil {
			last = st
			if st == "working" {
				sawWorking = true
			}
			observed := false
			if strings.TrimSpace(payloadText) != "" {
				if pane, readErr := PaneRead(target, 200); readErr == nil && taskTextObserved(payloadText, pane) {
					observed = true
				}
			}
			if observed || ConsumptionProvenSeen(baseline, st, sawWorking) {
				return operatorReadback{
					Version:        1,
					Key:            key,
					Target:         target,
					Session:        session,
					BaselineStatus: baseline,
					FinalStatus:    st,
					SequenceToken:  sequenceToken(baseline, st),
					Submitted:      true,
					Consumed:       true,
					Verified:       true,
					SawWorking:     sawWorking,
					PayloadSHA256:  payloadSHA,
				}, nil
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		if !nudged && time.Now().Add(poll).After(deadline.Add(-timeout/2)) {
			_ = SendKeys(target, "Enter")
			nudged = true
		}
		select {
		case <-ctx.Done():
			return operatorReadback{}, ctx.Err()
		case <-time.After(poll):
		}
	}
	return operatorReadback{}, fmt.Errorf("%w: no prompt-correlated consumption (key %q baseline %q last %q payload_sha256 %s)",
		textdelivery.ErrDurableAmbiguous, key, baseline, last, payloadSHA)
}

// DeliverOperator opens the shared receipt authority, reserves the exact
// direct-argv intent, invokes herdr with the payload as TEXT, and completes
// only after ConsumptionProvenSeen holds.
func DeliverOperator(ctx context.Context, d OperatorDelivery) (DeliveryProof, error) {
	return deliverOperator(ctx, d, textdelivery.NewDirectExecutor(nil))
}

// DeliverOperatorWithExecutor is the hermetic transport seam for tests.
func DeliverOperatorWithExecutor(ctx context.Context, d OperatorDelivery, executor textdelivery.Executor) (DeliveryProof, error) {
	return deliverOperator(ctx, d, executor)
}

func deliverOperator(ctx context.Context, d OperatorDelivery, executor textdelivery.Executor) (DeliveryProof, error) {
	if d.Key == "" || d.Target == "" || d.Generation <= 0 {
		return DeliveryProof{}, errors.New("herdr delivery: key, positive generation, and target are required")
	}
	if executor == nil {
		return DeliveryProof{}, errors.New("herdr delivery: executor is required")
	}
	if d.StatePath == "" {
		d.StatePath = filepath.Join(".herd", "herdr-delivery.db")
	}
	store, err := outbox.NewStore(d.StatePath)
	if err != nil {
		return DeliveryProof{}, fmt.Errorf("herdr delivery: open receipt authority: %w", err)
	}
	defer store.Close()

	body, err := d.Payload.Read()
	if err != nil {
		return DeliveryProof{}, err
	}
	if bytes.IndexByte(body, 0) >= 0 {
		return DeliveryProof{}, textdelivery.ErrNULPayload
	}

	// herdr agent prompt <TARGET> <TEXT> [ --wait --until working --timeout MS ]
	// TEXT is a single argv element (byte-identical); never shell-concatenated.
	args := []string{"agent", "prompt", d.Target, string(body)}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if d.Wait {
		args = append(args, "--wait", "--until", "working", "--timeout", fmt.Sprint(timeout.Milliseconds()))
	}

	ledger := textdelivery.NewDurableLedgerWithReadbackPolicy(store, d.Generation, operatorReadbackPolicy(d.Key, d.Target, d.Session))
	proofExecutor := textdelivery.ExecutorFunc(func(execCtx context.Context, command textdelivery.Command) ([]byte, error) {
		baseline, err := statusProbe(d.Target)
		if err != nil {
			// Agent not yet listed is a valid baseline (empty) for fresh launches.
			baseline = ""
		}
		if _, err := executor.Execute(execCtx, command); err != nil {
			return nil, err
		}
		// Herdr may acknowledge the text write before the pane composer handles
		// submission.  An immediate Enter makes the send-text/send-keys
		// sequence reliable (FAC-388); status polling remains authoritative.
		_ = SendKeys(d.Target, "Enter")
		proof, err := waitForOperatorConsumptionText(execCtx, d.Key, d.Target, d.Session, baseline, textdelivery.Digest(body), string(body), timeout)
		if err != nil {
			return nil, err
		}
		return json.Marshal(proof)
	})

	receipt, err := ledger.DeliverBound(ctx, d.Key, herdrCLI, args, textdelivery.Payload{Bytes: body}, canonicalHerdrIdentity(d.Target, d.Session), proofExecutor)
	if err != nil {
		return DeliveryProof{}, err
	}
	var readbackProof operatorReadback
	if err := json.Unmarshal(receipt.Readback, &readbackProof); err != nil {
		return DeliveryProof{}, fmt.Errorf("herdr delivery: corrupt completed proof: %w", err)
	}
	return DeliveryProof{
		Key: d.Key, Generation: receipt.Generation, Target: d.Target, Session: d.Session,
		Wait: d.Wait, Executable: herdrCLI, Argv: append([]string(nil), args...),
		PayloadSHA256: receipt.SHA256, IntentSHA256: receipt.IntentSHA256,
		ReadbackSHA256: textdelivery.Digest(receipt.Readback), Readback: append([]byte(nil), receipt.Readback...),
		Verified: readbackProof.Verified, Consumed: readbackProof.Consumed,
		BaselineStatus: readbackProof.BaselineStatus, FinalStatus: readbackProof.FinalStatus,
		SequenceToken: readbackProof.SequenceToken, SawWorking: readbackProof.SawWorking,
	}, nil
}
