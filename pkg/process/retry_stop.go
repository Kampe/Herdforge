package process

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// QuotaStopKey uniquely identifies an execution route for quota stop governance.
type QuotaStopKey struct {
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Model    string `json:"model"`
}

// String returns a canonical representation of the route key.
func (k QuotaStopKey) String() string {
	if k.Account != "" {
		return fmt.Sprintf("%s--%s--%s", k.Provider, k.Account, k.Model)
	}
	return fmt.Sprintf("%s--%s", k.Provider, k.Model)
}

// QuotaStopRecord captures an active bounded retry stop for an exhausted route.
type QuotaStopRecord struct {
	Key        QuotaStopKey `json:"key"`
	SessionID  string       `json:"session_id"`
	TurnID     string       `json:"turn_id"`
	Reason     string       `json:"reason"`
	RecordedAt time.Time    `json:"recorded_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	StopCount  int          `json:"stop_count"`
}

// QuotaRetryStopController governs bounded, idempotent retry stops for exhausted routes.
// It guarantees that repeated fresh observations are bounded and idempotent, while
// stale, mismatched, foreign, or malformed observations are refused.
type QuotaRetryStopController struct {
	mu            sync.Mutex
	stops         map[QuotaStopKey]*QuotaStopRecord
	defaultCool   time.Duration
	maxStopsRoute int
}

// NewQuotaRetryStopController initializes a new controller with default bounds.
func NewQuotaRetryStopController() *QuotaRetryStopController {
	return &QuotaRetryStopController{
		stops:         make(map[QuotaStopKey]*QuotaStopRecord),
		defaultCool:   15 * time.Minute,
		maxStopsRoute: 5,
	}
}

// RecordQuotaStop records a retry stop for an authoritative fresh quota failure.
// If evidence is stale, unauthenticated, mismatched, or malformed, it is refused with an error.
// If an active stop already exists for this exact turn, it returns the existing record idempotently.
func (c *QuotaRetryStopController) RecordQuotaStop(ev *TerminalEvidence, ctx SessionContext, cool time.Duration) (*QuotaStopRecord, bool, error) {
	if ev == nil {
		return nil, false, errors.New("terminal evidence is required")
	}

	// 1. Authoritative session, turn, route, and freshness validation
	if err := ev.Validate(ctx); err != nil {
		return nil, false, fmt.Errorf("refuse untrusted or stale quota evidence: %w", err)
	}

	// 2. Ensure evidence actually indicates a provider quota failure
	isQuota := strings.EqualFold(ev.FinishReason, "quota") ||
		ProviderExhaustionReason(ev.Error) != "" ||
		ProviderExhaustionReason(ev.Status) != ""
	if !isQuota {
		return nil, false, errors.New("terminal evidence does not indicate quota failure")
	}

	if cool <= 0 {
		cool = c.defaultCool
	}

	key := QuotaStopKey{
		Provider: strings.ToLower(ev.Provider),
		Account:  strings.ToLower(ev.Account),
		Model:    strings.ToLower(ev.Model),
	}

	now := ctx.Now
	if now.IsZero() {
		now = ev.Timestamp
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	existing, exists := c.stops[key]
	if exists && now.Before(existing.ExpiresAt) {
		// Existing active stop on this route
		if existing.SessionID == ev.SessionID && existing.TurnID == ev.TurnID {
			// Idempotent duplicate observation for the same session and turn
			return existing, false, nil
		}

		// New turn or session hitting the same exhausted route: bounded count increment
		if existing.StopCount < c.maxStopsRoute {
			existing.StopCount++
		}
		// Refresh expiration if new cooldown is further out
		newExp := now.Add(cool)
		if newExp.After(existing.ExpiresAt) {
			existing.ExpiresAt = newExp
		}
		return existing, false, nil
	}

	// Fresh stop creation
	rec := &QuotaStopRecord{
		Key:        key,
		SessionID:  ev.SessionID,
		TurnID:     ev.TurnID,
		Reason:     ev.Error,
		RecordedAt: now,
		ExpiresAt:  now.Add(cool),
		StopCount:  1,
	}
	if rec.Reason == "" {
		rec.Reason = "provider quota or rate limit reported"
	}
	c.stops[key] = rec
	return rec, true, nil
}

// IsRouteStopped reports whether a route is currently in an active retry stop.
func (c *QuotaRetryStopController) IsRouteStopped(key QuotaStopKey, now time.Time) (bool, string) {
	key.Provider = strings.ToLower(key.Provider)
	key.Account = strings.ToLower(key.Account)
	key.Model = strings.ToLower(key.Model)

	c.mu.Lock()
	defer c.mu.Unlock()

	rec, ok := c.stops[key]
	if !ok {
		return false, ""
	}
	if now.After(rec.ExpiresAt) {
		delete(c.stops, key)
		return false, ""
	}
	return true, rec.Reason
}

// ActiveStops returns a snapshot of all unexpired stops.
func (c *QuotaRetryStopController) ActiveStops(now time.Time) []*QuotaStopRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []*QuotaStopRecord
	for k, rec := range c.stops {
		if now.After(rec.ExpiresAt) {
			delete(c.stops, k)
			continue
		}
		cp := *rec
		out = append(out, &cp)
	}
	return out
}

var (
	defaultControllerLock sync.Mutex
	defaultController     *QuotaRetryStopController
)

// DefaultQuotaRetryStopController returns the singleton controller instance.
func DefaultQuotaRetryStopController() *QuotaRetryStopController {
	defaultControllerLock.Lock()
	defer defaultControllerLock.Unlock()
	if defaultController == nil {
		defaultController = NewQuotaRetryStopController()
	}
	return defaultController
}

// EvaluateAndRecordStop evaluates evidence and, if fresh authoritative quota exhaustion is confirmed,
// records a bounded idempotent retry stop using the default controller.
func EvaluateAndRecordStop(ev *TerminalEvidence, ctx SessionContext, rawText string, cool time.Duration) (EvaluationResult, *QuotaStopRecord, error) {
	res := EvaluateEvidence(ev, ctx, rawText)
	if res.Class != Quota || !res.Blocked || !res.ProviderDeath {
		return res, nil, nil
	}
	rec, _, err := DefaultQuotaRetryStopController().RecordQuotaStop(ev, ctx, cool)
	return res, rec, err
}
