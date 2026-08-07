package provider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// MutationCapability is a single-use, op-bound grant issued only after
// authoritative lease validation (mint credential). Workers never mint.
//
// Action is "status" (default) or "comment". Status binds status mutations;
// Comment binds exact comment body for comment mutations.
type MutationCapability struct {
	Repo        string `json:"repo"`
	Provider    string `json:"provider"`
	Project     string `json:"project"`
	TaskRef     string `json:"task_ref"`
	BoardTaskID string `json:"board_task_id"`
	Generation  int64  `json:"generation"`
	OwnerID     string `json:"owner_id"`
	OpID        string `json:"op_id"`
	Action      string `json:"action"` // status | comment
	Status      string `json:"status"`
	Comment     string `json:"comment,omitempty"`
	ExpiresUnix int64  `json:"expires_unix"`
	InstanceID  string `json:"instance_id"`
	Nonce       string `json:"nonce"`
	MAC         string `json:"mac"`
}

const (
	grantStatePending   = "pending"
	grantStateInFlight  = "in_flight"
	grantStateUpstream  = "upstream_committed"
	grantStateApplied   = "applied"
	grantStateAmbiguous = "ambiguous"

	mutationCapabilityHeader = "X-Herd-Lease-Capability"
	mintAuthHeader           = "X-Herd-Mint-Token"
	opScopeStatus            = "status"
	capActionStatus          = "status"
	capActionComment         = "comment"
)

func mutationCapabilityCanonical(c MutationCapability) string {
	action := c.Action
	if action == "" {
		action = capActionStatus
	}
	return strings.Join([]string{
		c.Repo,
		c.Provider,
		c.Project,
		c.TaskRef,
		c.BoardTaskID,
		strconv.FormatInt(c.Generation, 10),
		c.OwnerID,
		c.OpID,
		action,
		NormalizeStatus(c.Status),
		c.Comment,
		strconv.FormatInt(c.ExpiresUnix, 10),
		c.InstanceID,
		c.Nonce,
	}, "\x1f")
}

// MintMutationCapability signs a status-bound capability.
func MintMutationCapability(
	secret, instanceID string,
	key claim.LeaseKey,
	boardTaskID, ownerID, opID, status string,
	generation, expiresUnix int64,
) (string, error) {
	return mintCapability(secret, instanceID, key, boardTaskID, ownerID, opID, capActionStatus, status, "", generation, expiresUnix)
}

// MintCommentCapability signs a comment-body-bound capability.
func MintCommentCapability(
	secret, instanceID string,
	key claim.LeaseKey,
	boardTaskID, ownerID, opID, comment string,
	generation, expiresUnix int64,
) (string, error) {
	if comment == "" {
		return "", fmt.Errorf("provider: comment capability requires exact body")
	}
	return mintCapability(secret, instanceID, key, boardTaskID, ownerID, opID, capActionComment, "", comment, generation, expiresUnix)
}

func mintCapability(
	secret, instanceID string,
	key claim.LeaseKey,
	boardTaskID, ownerID, opID, action, status, comment string,
	generation, expiresUnix int64,
) (string, error) {
	if len(secret) < 16 || instanceID == "" {
		return "", fmt.Errorf("provider: mutation capability requires secret+instance")
	}
	if key.Repo == "" || key.Provider == "" || key.Project == "" || key.TaskRef == "" {
		return "", fmt.Errorf("provider: mutation capability requires full LeaseKey")
	}
	if ownerID == "" || opID == "" || generation <= 0 {
		return "", fmt.Errorf("provider: mutation capability requires owner, op, generation")
	}
	if action == capActionStatus && status == "" {
		return "", fmt.Errorf("provider: mutation capability requires exact status")
	}
	if action == capActionComment && comment == "" {
		return "", fmt.Errorf("provider: mutation capability requires exact comment")
	}
	if boardTaskID == "" {
		boardTaskID = key.TaskRef
	}
	if expiresUnix < time.Now().UTC().Unix() {
		return "", fmt.Errorf("provider: capability already expired")
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "", err
	}
	c := MutationCapability{
		Repo: key.Repo, Provider: key.Provider, Project: key.Project, TaskRef: key.TaskRef,
		BoardTaskID: boardTaskID, Generation: generation, OwnerID: ownerID, OpID: opID,
		Action: action, Status: NormalizeStatus(status), Comment: comment,
		ExpiresUnix: expiresUnix, InstanceID: instanceID, Nonce: hex.EncodeToString(nb[:]),
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(mutationCapabilityCanonical(c)))
	c.MAC = hex.EncodeToString(mac.Sum(nil))
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyMutationCapability checks status-bound capability.
func VerifyMutationCapability(secret, instanceID, raw string, boardTaskID, opID, status string, fence int64) (*MutationCapability, error) {
	return verifyCapability(secret, instanceID, raw, boardTaskID, opID, capActionStatus, status, "", fence)
}

// VerifyCommentCapability checks comment-bound capability.
func VerifyCommentCapability(secret, instanceID, raw string, boardTaskID, opID, comment string, fence int64) (*MutationCapability, error) {
	return verifyCapability(secret, instanceID, raw, boardTaskID, opID, capActionComment, "", comment, fence)
}

func verifyCapability(secret, instanceID, raw, boardTaskID, opID, action, status, comment string, fence int64) (*MutationCapability, error) {
	if raw == "" {
		return nil, fmt.Errorf("provider: missing mutation capability")
	}
	var c MutationCapability
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("provider: capability decode: %w", err)
	}
	cAction := c.Action
	if cAction == "" {
		cAction = capActionStatus
	}
	if cAction != action {
		return nil, fmt.Errorf("provider: capability action mismatch")
	}
	if c.BoardTaskID != boardTaskID {
		return nil, fmt.Errorf("provider: capability board_task_id mismatch")
	}
	if c.Generation != fence {
		return nil, fmt.Errorf("provider: capability generation %d != fence %d", c.Generation, fence)
	}
	if c.OpID != opID {
		return nil, fmt.Errorf("provider: capability op_id mismatch")
	}
	if action == capActionStatus && NormalizeStatus(c.Status) != NormalizeStatus(status) {
		return nil, fmt.Errorf("provider: capability status mismatch")
	}
	if action == capActionComment && c.Comment != comment {
		return nil, fmt.Errorf("provider: capability comment mismatch")
	}
	if c.InstanceID != instanceID {
		return nil, fmt.Errorf("provider: capability not issued by this broker instance")
	}
	if c.ExpiresUnix < time.Now().UTC().Unix() {
		return nil, fmt.Errorf("provider: capability expired")
	}
	if c.Repo == "" || c.Provider == "" || c.Project == "" || c.TaskRef == "" {
		return nil, fmt.Errorf("provider: capability missing full LeaseKey")
	}
	if c.Nonce == "" || c.MAC == "" || c.OwnerID == "" {
		return nil, fmt.Errorf("provider: capability missing nonce/mac/owner")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(mutationCapabilityCanonical(c)))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(c.MAC)
	if err != nil || !hmac.Equal(want, got) {
		return nil, fmt.Errorf("provider: capability MAC invalid")
	}
	return &c, nil
}

// LeaseKey returns the full canonical lease key bound in the capability.
func (c *MutationCapability) LeaseKey() claim.LeaseKey {
	if c == nil {
		return claim.LeaseKey{}
	}
	return claim.LeaseKey{Repo: c.Repo, Provider: c.Provider, Project: c.Project, TaskRef: c.TaskRef}
}
