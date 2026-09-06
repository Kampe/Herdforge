package dispatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	TaskPacketFile       = "TASK-PACKET.md"
	taskArtifactDir      = ".herd/receipts"
	taskArtifactFile     = "task-artifacts.json"
	taskArtifactLockFile = ".task-artifacts.lock"

	taskPacketBindingPrefix = "\n\n## Signed task-context binding\n\n```herd-task-context-binding-v1\n"
	taskPacketBindingSuffix = "\n```\n"
)

var (
	errTaskArtifactConflict = errors.New("dispatch task artifacts conflict")
	errTaskArtifactStale    = errors.New("dispatch task artifacts stale generation")
)

type taskArtifactPhase string

const (
	taskArtifactBeforePublish     taskArtifactPhase = "before_publish"
	taskArtifactContextTempCreate taskArtifactPhase = "context_temp_create"
	taskArtifactContextTempWrite  taskArtifactPhase = "context_temp_write"
	taskArtifactContextTempSync   taskArtifactPhase = "context_temp_sync"
	taskArtifactContextTempClose  taskArtifactPhase = "context_temp_close"
	taskArtifactPacketTempCreate  taskArtifactPhase = "packet_temp_create"
	taskArtifactPacketTempWrite   taskArtifactPhase = "packet_temp_write" // #nosec G101 -- fault-injection phase label, never a credential.
	taskArtifactPacketTempSync    taskArtifactPhase = "packet_temp_sync"
	taskArtifactPacketTempClose   taskArtifactPhase = "packet_temp_close"
	taskArtifactReceiptTempCreate taskArtifactPhase = "receipt_temp_create"
	taskArtifactReceiptTempWrite  taskArtifactPhase = "receipt_temp_write"
	taskArtifactReceiptTempSync   taskArtifactPhase = "receipt_temp_sync"
	taskArtifactReceiptTempClose  taskArtifactPhase = "receipt_temp_close"
	taskArtifactInvalidateReceipt taskArtifactPhase = "invalidate_receipt"
	taskArtifactContextRename     taskArtifactPhase = "context_rename"
	taskArtifactContextDirSync    taskArtifactPhase = "context_dir_sync"
	taskArtifactContextReadback   taskArtifactPhase = "context_readback"
	taskArtifactPacketRename      taskArtifactPhase = "packet_rename"
	taskArtifactPacketDirSync     taskArtifactPhase = "packet_dir_sync"
	taskArtifactPacketReadback    taskArtifactPhase = "packet_readback"
	taskArtifactReceiptRename     taskArtifactPhase = "receipt_rename"
	taskArtifactReceiptDirSync    taskArtifactPhase = "receipt_dir_sync"
	taskArtifactReceiptReadback   taskArtifactPhase = "receipt_readback"
	taskArtifactFinalValidation   taskArtifactPhase = "final_validation"
)

type taskArtifactIdentity struct {
	ProviderType    string `json:"provider_type"`
	ProjectID       string `json:"project_id"`
	TaskRef         string `json:"task_ref"`
	TaskID          string `json:"task_id"`
	BaseSHA         string `json:"base_sha"`
	Branch          string `json:"branch"`
	SessionID       string `json:"session_id"`
	LeaseID         string `json:"lease_id"`
	LeaseGeneration int64  `json:"lease_generation"`
}

type taskPacketBinding struct {
	Version int `json:"version"`
	taskArtifactIdentity
	ContextSHA256    string `json:"context_sha256"`
	ContextSignature string `json:"context_signature"`
}

type taskArtifactReceipt struct {
	Version int `json:"version"`
	taskArtifactIdentity
	PacketSHA256        string `json:"packet_sha256"`
	PacketPayloadSHA256 string `json:"packet_payload_sha256"`
	ContextSHA256       string `json:"context_sha256"`
}

type taskArtifactPublisher struct {
	fail func(taskArtifactPhase) error
}

type artifactStagePhases struct {
	create taskArtifactPhase
	write  taskArtifactPhase
	sync   taskArtifactPhase
	close  taskArtifactPhase
}

type artifactCommitPhases struct {
	rename   taskArtifactPhase
	dirSync  taskArtifactPhase
	readback taskArtifactPhase
}

type stagedTaskArtifact struct {
	tmpPath string
	path    string
	data    []byte
}

func taskArtifactIdentityFor(tc TaskContext) taskArtifactIdentity {
	return taskArtifactIdentity{
		ProviderType: tc.ProviderType, ProjectID: tc.ProjectID,
		TaskRef: tc.TaskRef, TaskID: tc.TaskID, BaseSHA: tc.BaseSHA,
		Branch: tc.Branch, SessionID: tc.SessionID, LeaseID: tc.LeaseID,
		LeaseGeneration: tc.LeaseGeneration,
	}
}

func (id taskArtifactIdentity) sameLaunch(other taskArtifactIdentity) bool {
	return id == other
}

func signedTaskContextBytes(tc TaskContext) ([]byte, error) {
	if err := tc.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tc.Signature) == "" {
		return nil, errors.New("task artifact context must be coordinator-signed")
	}
	data, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal task artifact context: %w", err)
	}
	return append(data, '\n'), nil
}

func taskArtifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func packetPayload(packet string) (string, error) {
	count := strings.Count(packet, taskPacketBindingPrefix)
	if count == 0 {
		return strings.TrimRight(packet, "\n"), nil
	}
	if count != 1 {
		return "", fmt.Errorf("task packet has %d signed binding sections", count)
	}
	start := strings.Index(packet, taskPacketBindingPrefix)
	rest := packet[start+len(taskPacketBindingPrefix):]
	end := strings.Index(rest, taskPacketBindingSuffix)
	if end < 0 || strings.TrimSpace(rest[end+len(taskPacketBindingSuffix):]) != "" {
		return "", errors.New("task packet signed binding is partial or not final")
	}
	return strings.TrimRight(packet[:start], "\n"), nil
}

func bindTaskPacket(packet string, tc TaskContext) (string, error) {
	contextData, err := signedTaskContextBytes(tc)
	if err != nil {
		return "", err
	}
	payload, err := packetPayload(packet)
	if err != nil {
		return "", err
	}
	binding := taskPacketBinding{
		Version: 1, taskArtifactIdentity: taskArtifactIdentityFor(tc),
		ContextSHA256: taskArtifactDigest(contextData), ContextSignature: tc.Signature,
	}
	bindingData, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal task packet binding: %w", err)
	}
	return payload + taskPacketBindingPrefix + string(bindingData) + taskPacketBindingSuffix, nil
}

func parseTaskPacketBinding(packet string) (taskPacketBinding, error) {
	var binding taskPacketBinding
	if strings.Count(packet, taskPacketBindingPrefix) != 1 {
		return binding, errors.New("task packet must contain exactly one signed context binding")
	}
	start := strings.Index(packet, taskPacketBindingPrefix) + len(taskPacketBindingPrefix)
	rest := packet[start:]
	end := strings.Index(rest, taskPacketBindingSuffix)
	if end < 0 || strings.TrimSpace(rest[end+len(taskPacketBindingSuffix):]) != "" {
		return binding, errors.New("task packet signed context binding is partial or not final")
	}
	if err := json.Unmarshal([]byte(rest[:end]), &binding); err != nil {
		return binding, fmt.Errorf("decode task packet binding: %w", err)
	}
	if binding.Version != 1 {
		return binding, fmt.Errorf("task packet binding version %d is unsupported", binding.Version)
	}
	return binding, nil
}

func (p taskArtifactPublisher) at(phase taskArtifactPhase) error {
	if p.fail == nil {
		return nil
	}
	if err := p.fail(phase); err != nil {
		return fmt.Errorf("task artifact phase %s: %w", phase, err)
	}
	return nil
}

func lockTaskArtifacts(worktreePath string, how int) (*os.File, error) {
	dir := filepath.Join(worktreePath, taskArtifactDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create task artifact runtime directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, taskArtifactLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open task artifact lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), how); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock task artifacts: %w", err)
	}
	return lock, nil
}

func unlockTaskArtifacts(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func (p taskArtifactPublisher) stage(worktreePath, name string, data []byte, phases artifactStagePhases) (*stagedTaskArtifact, error) {
	if err := p.at(phases.create); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(worktreePath, "."+name+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("stage %s: %w", name, err)
	}
	stage := &stagedTaskArtifact{tmpPath: tmp.Name(), path: filepath.Join(worktreePath, name), data: data}
	fail := func(step string, cause error) (*stagedTaskArtifact, error) {
		_ = tmp.Close()
		_ = os.Remove(stage.tmpPath)
		return nil, fmt.Errorf("%s %s: %w", step, name, cause)
	}
	if err := p.at(phases.write); err != nil {
		return fail("write", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write", err)
	}
	if err := p.at(phases.sync); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := p.at(phases.close); err != nil {
		return fail("close", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close", err)
	}
	if err := os.Chmod(stage.tmpPath, 0o644); err != nil {
		_ = os.Remove(stage.tmpPath)
		return nil, fmt.Errorf("chmod %s: %w", name, err)
	}
	return stage, nil
}

func (s *stagedTaskArtifact) cleanup() {
	if s != nil {
		_ = os.Remove(s.tmpPath)
	}
}

func syncTaskArtifactDir(worktreePath string) error {
	dir, err := os.Open(worktreePath)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}

func (p taskArtifactPublisher) commit(worktreePath string, stage *stagedTaskArtifact, phases artifactCommitPhases) error {
	if err := p.at(phases.rename); err != nil {
		return err
	}
	if err := os.Rename(stage.tmpPath, stage.path); err != nil {
		return fmt.Errorf("commit %s: %w", filepath.Base(stage.path), err)
	}
	if err := p.at(phases.dirSync); err != nil {
		return err
	}
	if err := syncTaskArtifactDir(worktreePath); err != nil {
		return fmt.Errorf("sync task artifact directory: %w", err)
	}
	if err := p.at(phases.readback); err != nil {
		return err
	}
	got, err := os.ReadFile(stage.path)
	if err != nil {
		return fmt.Errorf("read back %s: %w", filepath.Base(stage.path), err)
	}
	if !bytes.Equal(got, stage.data) {
		return fmt.Errorf("read back %s digest %s, want %s", filepath.Base(stage.path), taskArtifactDigest(got), taskArtifactDigest(stage.data))
	}
	return nil
}

func removeTaskArtifactReceipt(worktreePath string) error {
	dir := filepath.Join(worktreePath, taskArtifactDir)
	err := os.Remove(filepath.Join(dir, taskArtifactFile))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove task artifact receipt: %w", err)
	}
	if err == nil {
		if syncErr := syncTaskArtifactDir(dir); syncErr != nil {
			return fmt.Errorf("sync removed task artifact receipt: %w", syncErr)
		}
	}
	return nil
}

func invalidateTaskArtifacts(worktreePath string) error {
	lock, err := lockTaskArtifacts(worktreePath, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlockTaskArtifacts(lock)
	return removeTaskArtifactReceipt(worktreePath)
}

func readTaskArtifactReceipt(worktreePath string) (*taskArtifactReceipt, error) {
	data, err := os.ReadFile(filepath.Join(worktreePath, taskArtifactDir, taskArtifactFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read task artifact receipt: %w", err)
	}
	var receipt taskArtifactReceipt
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.Version != 1 {
		// A partial/corrupt commit receipt authorizes no launch. Preserve it as
		// evidence and let a generation-fenced retry replace it under the lock.
		return nil, nil
	}
	return &receipt, nil
}

func existingTaskArtifactIdentity(worktreePath string) (*taskArtifactIdentity, *taskArtifactReceipt, error) {
	receipt, err := readTaskArtifactReceipt(worktreePath)
	if err != nil {
		return nil, nil, err
	}
	if receipt != nil {
		id := receipt.taskArtifactIdentity
		return &id, receipt, nil
	}
	data, err := os.ReadFile(filepath.Join(worktreePath, TaskContextFile))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read existing task context fence: %w", err)
	}
	var tc TaskContext
	if json.Unmarshal(data, &tc) != nil || tc.Validate() != nil {
		return nil, nil, nil
	}
	id := taskArtifactIdentityFor(tc)
	return &id, nil, nil
}

func (p taskArtifactPublisher) Publish(worktreePath, packet string, tc TaskContext) (published string, retErr error) {
	boundPacket, err := bindTaskPacket(packet, tc)
	if err != nil {
		return "", err
	}
	contextData, err := signedTaskContextBytes(tc)
	if err != nil {
		return "", err
	}
	payload, err := packetPayload(boundPacket)
	if err != nil {
		return "", err
	}
	packetData := []byte(boundPacket)
	receipt := taskArtifactReceipt{
		Version: 1, taskArtifactIdentity: taskArtifactIdentityFor(tc),
		PacketSHA256:        taskArtifactDigest(packetData),
		PacketPayloadSHA256: taskArtifactDigest([]byte(payload)),
		ContextSHA256:       taskArtifactDigest(contextData),
	}
	receiptData, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal task artifact receipt: %w", err)
	}
	receiptData = append(receiptData, '\n')

	lock, err := lockTaskArtifacts(worktreePath, syscall.LOCK_EX)
	if err != nil {
		return "", err
	}
	defer unlockTaskArtifacts(lock)

	existingID, existingReceipt, err := existingTaskArtifactIdentity(worktreePath)
	if err != nil {
		return "", err
	}
	if existingID != nil {
		if existingID.LeaseGeneration > receipt.LeaseGeneration {
			return "", fmt.Errorf("%w: existing generation %d > incoming %d", errTaskArtifactStale, existingID.LeaseGeneration, receipt.LeaseGeneration)
		}
		if existingID.LeaseGeneration == receipt.LeaseGeneration && !existingID.sameLaunch(receipt.taskArtifactIdentity) {
			return "", fmt.Errorf("%w: generation %d already belongs to session %s lease %s", errTaskArtifactConflict, receipt.LeaseGeneration, existingID.SessionID, existingID.LeaseID)
		}
	}
	if existingReceipt != nil && existingReceipt.taskArtifactIdentity.sameLaunch(receipt.taskArtifactIdentity) {
		if existingReceipt.PacketPayloadSHA256 != receipt.PacketPayloadSHA256 {
			return "", fmt.Errorf("%w: generation %d attempted to change packet payload", errTaskArtifactConflict, receipt.LeaseGeneration)
		}
		if existingReceipt.PacketSHA256 == receipt.PacketSHA256 && existingReceipt.ContextSHA256 == receipt.ContextSHA256 {
			if err := validateTaskArtifactsLocked(worktreePath, boundPacket, tc, nil); err == nil {
				return boundPacket, nil
			}
		}
	}
	// Once an incoming generation has passed the stale/conflict fences, any
	// publication failure must revoke the prior consumer authorization. Keep
	// the packet/context bytes as attributable evidence, but leave no receipt
	// through which a production launcher could accept either generation.
	publicationAttempted := true
	defer func() {
		if retErr != nil && publicationAttempted {
			retErr = errors.Join(retErr, removeTaskArtifactReceipt(worktreePath))
		}
	}()
	if err := p.at(taskArtifactBeforePublish); err != nil {
		return "", err
	}

	contextStage, err := p.stage(worktreePath, TaskContextFile, contextData, artifactStagePhases{
		create: taskArtifactContextTempCreate, write: taskArtifactContextTempWrite,
		sync: taskArtifactContextTempSync, close: taskArtifactContextTempClose,
	})
	if err != nil {
		return "", err
	}
	defer contextStage.cleanup()
	packetStage, err := p.stage(worktreePath, TaskPacketFile, packetData, artifactStagePhases{
		create: taskArtifactPacketTempCreate, write: taskArtifactPacketTempWrite,
		sync: taskArtifactPacketTempSync, close: taskArtifactPacketTempClose,
	})
	if err != nil {
		return "", err
	}
	defer packetStage.cleanup()
	receiptDir := filepath.Join(worktreePath, taskArtifactDir)
	receiptStage, err := p.stage(receiptDir, taskArtifactFile, receiptData, artifactStagePhases{
		create: taskArtifactReceiptTempCreate, write: taskArtifactReceiptTempWrite,
		sync: taskArtifactReceiptTempSync, close: taskArtifactReceiptTempClose,
	})
	if err != nil {
		return "", err
	}
	defer receiptStage.cleanup()

	if err := p.at(taskArtifactInvalidateReceipt); err != nil {
		return "", err
	}
	if err := removeTaskArtifactReceipt(worktreePath); err != nil {
		return "", err
	}

	if err := p.commit(worktreePath, contextStage, artifactCommitPhases{
		rename: taskArtifactContextRename, dirSync: taskArtifactContextDirSync, readback: taskArtifactContextReadback,
	}); err != nil {
		return "", err
	}
	if err := p.commit(worktreePath, packetStage, artifactCommitPhases{
		rename: taskArtifactPacketRename, dirSync: taskArtifactPacketDirSync, readback: taskArtifactPacketReadback,
	}); err != nil {
		return "", err
	}
	if err := p.commit(receiptDir, receiptStage, artifactCommitPhases{
		rename: taskArtifactReceiptRename, dirSync: taskArtifactReceiptDirSync, readback: taskArtifactReceiptReadback,
	}); err != nil {
		return "", err
	}
	if err := p.at(taskArtifactFinalValidation); err != nil {
		return "", err
	}
	if err := validateTaskArtifactsLocked(worktreePath, boundPacket, tc, nil); err != nil {
		return "", err
	}
	publicationAttempted = false
	return boundPacket, nil
}

func validateTaskArtifacts(worktreePath, expectedPacket string, expectedContext TaskContext, verifier *Verifier) error {
	lock, err := lockTaskArtifacts(worktreePath, syscall.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlockTaskArtifacts(lock)
	return validateTaskArtifactsLocked(worktreePath, expectedPacket, expectedContext, verifier)
}

func validateTaskArtifactsLocked(worktreePath, expectedPacket string, expectedContext TaskContext, verifier *Verifier) error {
	receipt, err := readTaskArtifactReceipt(worktreePath)
	if err != nil {
		return err
	}
	if receipt == nil {
		return errors.New("no usable task artifact commit receipt")
	}
	packetData, err := os.ReadFile(filepath.Join(worktreePath, TaskPacketFile))
	if err != nil {
		return fmt.Errorf("read committed task packet: %w", err)
	}
	contextData, err := os.ReadFile(filepath.Join(worktreePath, TaskContextFile))
	if err != nil {
		return fmt.Errorf("read committed task context: %w", err)
	}
	if taskArtifactDigest(packetData) != receipt.PacketSHA256 || taskArtifactDigest(contextData) != receipt.ContextSHA256 {
		return errors.New("task artifact receipt digest mismatch")
	}
	if expectedPacket != "" && string(packetData) != expectedPacket {
		return errors.New("task packet readback differs from dispatch payload")
	}
	var tc TaskContext
	if err := json.Unmarshal(contextData, &tc); err != nil {
		return fmt.Errorf("decode committed task context: %w", err)
	}
	if err := tc.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(tc.Signature) == "" {
		return errors.New("committed task context is unsigned")
	}
	if verifier != nil {
		if err := verifier.Verify(tc); err != nil {
			return fmt.Errorf("verify committed task context: %w", err)
		}
	}
	if !receipt.taskArtifactIdentity.sameLaunch(taskArtifactIdentityFor(tc)) {
		return errors.New("task artifact receipt identity differs from signed context")
	}
	if expectedContext.TaskRef != "" {
		expectedData, err := signedTaskContextBytes(expectedContext)
		if err != nil {
			return err
		}
		if !bytes.Equal(contextData, expectedData) {
			return errors.New("task context readback differs from dispatch authority")
		}
	}
	binding, err := parseTaskPacketBinding(string(packetData))
	if err != nil {
		return err
	}
	if !binding.taskArtifactIdentity.sameLaunch(taskArtifactIdentityFor(tc)) ||
		binding.ContextSHA256 != receipt.ContextSHA256 || binding.ContextSignature != tc.Signature {
		return errors.New("task packet binding differs from signed context")
	}
	payload, err := packetPayload(string(packetData))
	if err != nil {
		return err
	}
	if taskArtifactDigest([]byte(payload)) != receipt.PacketPayloadSHA256 {
		return errors.New("task packet payload digest mismatch")
	}
	return validateGeneratedPacketFields(string(packetData), tc)
}

func validateGeneratedPacketFields(packet string, tc TaskContext) error {
	lease := fmt.Sprint(tc.LeaseGeneration)
	required := []struct {
		prefix string
		line   string
	}{
		{prefix: "Worktree: ", line: "Worktree: current directory (Herdr cwd-enforced), branch " + tc.Branch + ". Work ONLY here — never edit files outside it."},
		{prefix: "Read the full spec yourself ", line: "Read the full spec yourself (do not wait for it inline) via the receipt-gated broker (provider=" + tc.ProviderType + " project=" + tc.ProjectID + "):"},
		{prefix: "  herd task get ", line: "  herd task get " + tc.TaskRef + " --full"},
		{prefix: "  Completion callback: ", line: "  Completion callback: herd shot " + tc.TaskRef + " --report complete --sha <sha> --lease " + lease},
		{prefix: "  BLOCKED: ", line: "  BLOCKED: herd shot " + tc.TaskRef + " --report blocked --detail \"<why>\" --lease " + lease},
	}
	lines := strings.Split(packet, "\n")
	for _, field := range required {
		matches := 0
		for _, line := range lines {
			if strings.HasPrefix(line, field.prefix) {
				matches++
				if line != field.line {
					return fmt.Errorf("task packet generated field mismatch: got %q, want %q", line, field.line)
				}
			}
		}
		if matches != 1 {
			return fmt.Errorf("task packet generated field %q occurs %d times", field.prefix, matches)
		}
	}
	assignmentPrefix := "ASSIGNMENT ENVELOPE: ADDRESSED ASSIGNMENT; issuer: "
	assignmentTail := "; task_ref: " + tc.TaskRef + "; task_id: " + tc.TaskID + "; lease_generation: " + lease + "; ASSIGNMENT ENVELOPE END."
	assignmentMatches := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, assignmentPrefix) {
			continue
		}
		assignmentMatches++
		if !strings.HasSuffix(line, assignmentTail) || strings.Count(line, "lease_generation:") != 1 {
			return fmt.Errorf("task packet assignment identity mismatch: %q", line)
		}
	}
	if assignmentMatches != 1 {
		return fmt.Errorf("task packet assignment envelope occurs %d times", assignmentMatches)
	}
	return nil
}
