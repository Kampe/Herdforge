package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kampe/Herdforge/pkg/claim"
)

func (k *KaneoProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return k.UpdateStatus(ctx, taskID, StatusInProgress)
}

// ListLiveComments is authoritative live-provider comment readback (CLI or
// HTTP). NEVER substitutes local AuthBroker receipts. Failures return error
// (EffectUnknown); empty success means ABSENT.
func (k *KaneoProvider) ListLiveComments(ctx context.Context, taskID string) ([]string, error) {
	if k == nil {
		return nil, fmt.Errorf("kaneo: nil provider")
	}
	if k.UseCLI {
		res, err := kaneoRunCLI(ctx, "kaneo", "task", "comment", "list", taskID, "--json")
		if err != nil {
			return nil, fmt.Errorf("kaneo comment list: %w", err)
		}
		return decodeKaneoCommentBodies(res.Stdout)
	}
	// Production Kaneo activity feed for comments.
	url := fmt.Sprintf("%s/api/activity/%s", k.APIURL, taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kaneo comment list: HTTP %d", resp.StatusCode)
	}
	return decodeKaneoCommentBodies(body)
}

func decodeKaneoCommentBodies(raw []byte) ([]string, error) {
	// Shape: [{ "content": "...", "type": "comment" }, ...] or { "comments": [...] }
	var arr []struct {
		Content string `json:"content"`
		Body    string `json:"body"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		var out []string
		for _, c := range arr {
			if c.Type != "" && c.Type != "comment" {
				continue
			}
			text := c.Content
			if text == "" {
				text = c.Body
			}
			if text != "" {
				out = append(out, text)
			}
		}
		return out, nil
	}
	var wrap struct {
		Comments []struct {
			Content string `json:"content"`
			Body    string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode comment list: %w", err)
	}
	var out []string
	for _, c := range wrap.Comments {
		text := c.Content
		if text == "" {
			text = c.Body
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return out, nil
}

func (k *KaneoProvider) requireCAS(ctx context.Context) error {
	if k == nil || !k.RequireCASMeta {
		return nil
	}
	if casOpID(ctx) == "" {
		return fmt.Errorf("kaneo: mutation refused without X-Herd-Op (unfenced bypass; FAC-147 fail-closed)")
	}
	if _, ok := casFenceToken(ctx); !ok {
		return fmt.Errorf("kaneo: mutation refused without X-Herd-Fence (unfenced bypass; FAC-147 fail-closed)")
	}
	return nil
}

// fenceEnv returns HERD_FENCE/HERD_OP env entries for CLI transport.
func fenceEnv(ctx context.Context) []string {
	var env []string
	if op := casOpID(ctx); op != "" {
		env = append(env, "HERD_OP="+op)
	}
	if tok, ok := casFenceToken(ctx); ok {
		env = append(env, "HERD_FENCE="+fmtInt64(tok))
	}
	return env
}

// runCLIMutate invokes Kaneo CLI with fence/op env attached (not dropped).
// When CAS meta is present, HERD_FENCE/HERD_OP are injected via env so
// use_cli: true does not strip fence transport (FAC-147).
func (k *KaneoProvider) runCLIMutate(ctx context.Context, args ...string) error {
	env := fenceEnv(ctx)
	var res *CLIResult
	var err error
	if len(env) > 0 {
		res, err = kaneoRunCLIEnv(ctx, env, "kaneo", args...)
	} else {
		// Unfenced path (tests / non-stack callers): preserve kaneoRunCLI hook.
		res, err = kaneoRunCLI(ctx, "kaneo", args...)
	}
	if err != nil {
		msg := cliErrMsg(res)
		label := "task"
		if len(args) > 0 {
			label = args[0]
		}
		if msg != "" {
			return fmt.Errorf("kaneo %s: %s: %w", label, msg, err)
		}
		return fmt.Errorf("kaneo %s: %w", label, err)
	}
	return nil
}

// withReceiver gates backend mutate through the authoritative AuthBroker.
// Fail-closed without a receiver when RequireCASMeta is set. Execute
// records in_progress before remote and reconciles crash retries without
// blind re-mutate (FAC-147 provider-success/local-failure window).
func (k *KaneoProvider) withReceiver(ctx context.Context, taskID string, expStatus, expComment string, backend func(ctx context.Context) error) error {
	if err := k.requireCAS(ctx); err != nil {
		return err
	}
	opID := casOpID(ctx)
	fence, hasFence := casFenceToken(ctx)
	if k.RequireCASMeta && k.Receiver == nil {
		return fmt.Errorf("kaneo: authoritative receiver required for fenced mutates (FAC-147 fail-closed)")
	}
	// Production fenced status requires a live FenceBroker (or hermetic
	// AtomicFenceServer). Fail closed before any remote/readback side effects.
	if expStatus != "" && (k.RequireCASMeta || (opID != "" && hasFence)) {
		if k.FenceBroker == nil && !k.AtomicFenceServer {
			return fmt.Errorf("%w: fenced UpdateStatus requires live FenceBroker (herd fence-broker + HERD_FENCE_BROKER_URL); stock Kaneo has no server-side fence+op-dedupe",
				claim.ErrProviderAmbiguous)
		}
	}
	if k.Receiver == nil {
		// Non-production / unfenced test path only.
		return backend(ctx)
	}
	if opID == "" || !hasFence {
		return fmt.Errorf("kaneo: fence+op required for authoritative receiver")
	}
	effectMet := func(ctx context.Context) (EffectState, error) {
		if expStatus == "" && expComment == "" {
			return EffectAbsent, nil
		}
		if expStatus != "" {
			// EffectPresent REQUIRES server-native opID(+fence) metadata from the
			// enforcing broker. GetTask.Status equality alone is NEVER sufficient
			// (foreign same-status false attribution). If op metadata cannot be
			// queried, return Unknown (BLOCKED) — not Present or Absent-from-status.
			if opID == "" {
				return EffectUnknown, nil
			}
			if k.FenceBroker == nil {
				// No broker readback surface → cannot admit Present; block recovery.
				return EffectUnknown, nil
			}
			ok, err := k.FenceBroker.OpApplied(ctx, opID, taskID, expStatus)
			if err != nil {
				return EffectUnknown, nil
			}
			if ok {
				return EffectPresent, nil
			}
			// Queried successfully: op not bound → Absent (may re-mutate under
			// ServerOpDedupe). Distinct from Unknown (query unavailable).
			return EffectAbsent, nil
		}
		if expComment != "" {
			live, err := k.ListLiveComments(ctx, taskID)
			if err != nil {
				return EffectUnknown, nil
			}
			for _, c := range live {
				if MatchCommentOp(c, expComment, opID) {
					return EffectPresent, nil
				}
			}
			return EffectAbsent, nil
		}
		return EffectAbsent, nil
	}
	return k.Receiver.Execute(ctx, taskID, fence, opID, expStatus, expComment, backend, effectMet)
}

func (k *KaneoProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	// Write the canonical lifecycle status so readback compares like-for-like
	// against dtoToTask/NormalizeStatus (production Kaneo CLI + HTTP).
	canonical := NormalizeStatus(status)
	dls := k.deadlines()
	writeCtx, cancel := WithOpDeadline(ctx, dls, OpMutate)
	writeErr := k.updateStatusOnce(writeCtx, taskID, canonical)
	cancel()
	if writeErr != nil {
		writeErr = AsTimeout("kaneo", "UpdateStatus", OpMutate, dls.For(OpMutate), writeErr)
	}
	// Parent ctx (not the expired write child) for readback / reconcile.
	return AfterMutation(ctx, k, dls, "kaneo", "UpdateStatus", taskID, canonical, writeErr)
}

func (k *KaneoProvider) updateStatusOnce(ctx context.Context, taskID, status string) error {
	expStatus := status
	if s := casExpectedStatus(ctx); s != "" {
		expStatus = s
	}
	return k.withReceiver(ctx, taskID, expStatus, "", func(ctx context.Context) error {
		return k.mutateStatus(ctx, taskID, status)
	})
}

// mutateStatus performs the remote status write.
// Production fenced path: MUST go through FenceBroker (atomic fence+op+dedupe
// with server-native op readback). Stock Kaneo alone is fail-closed.
func (k *KaneoProvider) mutateStatus(ctx context.Context, taskID, status string) error {
	opID := casOpID(ctx)
	fence, hasFence := casFenceToken(ctx)
	fenced := k.RequireCASMeta || (opID != "" && hasFence)

	if fenced {
		if opID == "" || !hasFence || fence <= 0 {
			return fmt.Errorf("kaneo: fenced status requires fence+op meta")
		}
		// Production: FenceBroker worker client. Capability must be pre-minted
		// by a coordinator FenceBrokerMinter (or unexported minter on this
		// process). Worker clients never mint.
		if k.FenceBroker != nil {
			// Immutable per-op capability from coordinator minter + ctx MintIdentity.
			var capability string
			if k.minter != nil {
				id, ok := MintIdentityFrom(ctx)
				if !ok {
					return fmt.Errorf("kaneo: coordinator mint requires WithMintIdentity on ctx (immutable per call)")
				}
				issued, err := k.minter.IssueCapability(ctx, CapabilityIssueRequest{
					BoardTaskID: taskID,
					TaskRef:     id.TaskRef,
					Repo:        id.Repo,
					Provider:    id.Provider,
					Project:     id.Project,
					OwnerID:     id.OwnerID,
					Generation:  fence,
					OpID:        opID,
					Status:      status,
				})
				if err != nil {
					return fmt.Errorf("kaneo: coordinator mint: %w", err)
				}
				capability = issued
			}
			if capability == "" {
				return fmt.Errorf("kaneo: pre-minted capability required (workers cannot mint; coordinator uses FenceBrokerMinter)")
			}
			return k.FenceBroker.MutateStatus(ctx, taskID, status, fence, opID, capability)
		}
		if !k.AtomicFenceServer {
			return fmt.Errorf("%w: fenced UpdateStatus requires live FenceBroker (herd fence-broker + HERD_FENCE_BROKER_URL); stock Kaneo has no server-side fence+op-dedupe",
				claim.ErrProviderAmbiguous)
		}
		// AtomicFenceServer without FenceBroker: direct HTTP to enforcing board (tests).
	}

	// AtomicFenceServer without FenceBroker: full-schema PUT (hermetic boards).
	if k.AtomicFenceServer {
		return k.mutateStatusFullSchemaPUT(ctx, taskID, status)
	}

	// Unfenced path: stock Kaneo PATCH (main production shape).
	if k.UseCLI {
		args := []string{"task", "status", taskID, status}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		return k.runCLIMutate(ctx, args...)
	}
	url := fmt.Sprintf("%s/api/task/%s", strings.TrimRight(strings.TrimSpace(k.APIURL), "/"), taskID)
	payload := map[string]string{"status": status}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	AttachFenceHeaders(ctx, req.Header.Set)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "UpdateStatus"
		}
		return err
	}
	return nil
}

// mutateStatusFullSchemaPUT rebuilds a full task PUT body for hermetic /
// broker-upstream boards that reject PATCH. Position is only included when
// GetTask reported HasPosition — never clobber board rank with a zero default.
// Safe for concurrent callers: does not toggle shared AtomicFenceServer.
func (k *KaneoProvider) mutateStatusFullSchemaPUT(ctx context.Context, taskID, status string) error {
	apiURL := strings.TrimSpace(k.APIURL)
	if apiURL == "" {
		return fmt.Errorf("kaneo: APIURL required for AtomicFenceServer mutate")
	}
	payload := map[string]any{"status": status}
	if cur, err := k.GetTask(ctx, taskID); err == nil && cur != nil {
		title := cur.Title
		if title == "" {
			title = taskID
		}
		priority := string(cur.Priority)
		if priority == "" {
			priority = "medium"
		}
		projectID := cur.ProjectID
		if projectID == "" {
			projectID = k.ProjectID
		}
		payload = map[string]any{
			"title": title, "description": cur.Description, "status": status,
			"priority": priority, "projectId": projectID,
		}
		if cur.HasPosition {
			payload["position"] = cur.Position
		}
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/task/%s", strings.TrimRight(apiURL, "/"), url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	k.authorizeKaneo(req)
	AttachFenceHeaders(ctx, req.Header.Set)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "UpdateStatus"
			if pe.StatusCode == http.StatusConflict {
				return fmt.Errorf("%w: %v", claim.ErrProviderFenceRejected, pe)
			}
		}
		return err
	}
	return nil
}

func (k *KaneoProvider) mutateStatusRemote(ctx context.Context, taskID, status string) error {
	return k.mutateStatus(ctx, taskID, status)
}

// mutateStatusAtomic is removed as a production concept: stock Kaneo has no
// atomic status+receipt primitive. Alias retained for test call sites.
func (k *KaneoProvider) mutateStatusAtomic(ctx context.Context, taskID, status string) error {
	return k.mutateStatus(ctx, taskID, status)
}

// postCommentDirect posts a comment without withReceiver (already inside Execute).
func (k *KaneoProvider) postCommentDirect(ctx context.Context, taskID, body string) error {
	if k.UseCLI {
		args := []string{"task", "comment", "add", taskID, body}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		return k.runCLIMutate(ctx, args...)
	}
	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, url.PathEscape(taskID))
	payload := map[string]string{"body": body}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	AttachFenceHeaders(ctx, req.Header.Set)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	if err := DecodeJSONResponse(resp, nil); err != nil {
		if pe, ok := err.(*ProviderError); ok {
			pe.Provider = "kaneo"
			pe.Op = "AddComment"
			if pe.StatusCode == http.StatusConflict {
				return fmt.Errorf("%w: %v", claim.ErrProviderFenceRejected, pe)
			}
		}
		return err
	}
	return nil
}

func (k *KaneoProvider) AddComment(ctx context.Context, taskID string, body string) error {
	dls := k.deadlines()
	ctx, cancel := WithOpDeadline(ctx, dls, OpComment)
	defer cancel()

	err := k.addCommentOnce(ctx, taskID, body)
	if err == nil {
		return nil
	}
	err = AsTimeout("kaneo", "AddComment", OpComment, dls.For(OpComment), err)
	if IsTimeout(err) {
		// Comments are not status-reconcilable; never blind-retry.
		return &AmbiguousMutationError{
			Provider: "kaneo",
			Op:       "AddComment",
			TaskID:   taskID,
			WriteErr: err,
		}
	}
	return err
}

func (k *KaneoProvider) addCommentOnce(ctx context.Context, taskID, body string) error {
	// Op-bound body for live readback identity (hold inb04ouq).
	postBody := body
	if op := casOpID(ctx); op != "" {
		postBody = CommentOpTaggedBody(body, op)
	}
	expComment := postBody
	if c := casExpectedComment(ctx); c != "" {
		if op := casOpID(ctx); op != "" {
			expComment = CommentOpTaggedBody(c, op)
		} else {
			expComment = c
		}
	}
	opID := casOpID(ctx)
	fence, hasFence := casFenceToken(ctx)
	fenced := k.RequireCASMeta || (opID != "" && hasFence)
	if fenced {
		if opID == "" || !hasFence || fence <= 0 {
			return fmt.Errorf("kaneo: fenced AddComment requires fence+op meta")
		}
		if k.FenceBroker != nil {
			var capability string
			if k.minter != nil {
				id, ok := MintIdentityFrom(ctx)
				if !ok {
					return fmt.Errorf("kaneo: coordinator mint requires WithMintIdentity for comments")
				}
				issued, err := k.minter.IssueCapability(ctx, CapabilityIssueRequest{
					BoardTaskID: taskID, TaskRef: id.TaskRef, Repo: id.Repo, Provider: id.Provider,
					Project: id.Project, OwnerID: id.OwnerID, Generation: fence, OpID: opID,
					Action: capActionComment, Comment: postBody,
				})
				if err != nil {
					return fmt.Errorf("kaneo: comment mint: %w", err)
				}
				capability = issued
			}
			if capability == "" {
				return fmt.Errorf("kaneo: pre-minted comment capability required (workers cannot mint)")
			}
			return k.FenceBroker.MutateComment(ctx, taskID, postBody, fence, opID, capability)
		}
		if !k.AtomicFenceServer {
			return fmt.Errorf("%w: fenced AddComment requires live FenceBroker; stock Kaneo has no server-side op-dedupe",
				claim.ErrProviderAmbiguous)
		}
	}
	return k.withReceiver(ctx, taskID, "", expComment, func(ctx context.Context) error {
		return k.addCommentRaw(ctx, taskID, postBody)
	})
}

// addCommentRaw posts a comment to stock Kaneo (broker upstream or hermetic
// AtomicFenceServer path). Fence headers attached when CAS meta is present.
func (k *KaneoProvider) addCommentRaw(ctx context.Context, taskID, body string) error {
	if k.UseCLI {
		args := []string{"task", "comment", "add", taskID, body}
		if k.ProjectID != "" {
			args = append(args, "--project", k.ProjectID)
		}
		return k.runCLIMutate(ctx, args...)
	}
	url := fmt.Sprintf("%s/api/task/%s/comment", k.APIURL, taskID)
	buf, _ := json.Marshal(map[string]string{"body": body})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	AttachFenceHeaders(ctx, req.Header.Set)
	resp, err := k.httpClient().Do(req)
	if err != nil {
		return err
	}
	return DecodeJSONResponse(resp, nil)
}
