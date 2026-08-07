package verifier

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hermeticContainerUser         = "65532:65532"
	hermeticPIDLimit              = 64
	hermeticMemoryLimit           = "512m"
	hermeticMemoryBytes           = 512 << 20
	hermeticVerifierTimeout       = 10 * time.Minute
	hermeticSetupTeardownMargin   = 5 * time.Minute
	hermeticTimeout               = hermeticVerifierTimeout + hermeticSetupTeardownMargin
	hermeticSourcePath            = "/tmp/build/source"
	hermeticBuildPath             = "/tmp/build"
	hermeticRunPath               = "/tmp/run"
	hermeticReplayPath            = "/tmp/replay"
	hermeticReceiptPath           = "/tmp/replay/receipt.json"
	hermeticTestCount             = "1"
	hermeticTestTimeout           = "10m"
	maxHermeticSourceArchiveBytes = 64 << 20
	maxHermeticCacheTarBytes      = 8 << 20
	maxHermeticReceiptTarBytes    = 64 << 10
	maxHermeticMountInfoBytes     = 64 << 10
	hermeticSourceTransportRoot   = "source"
)

var hermeticDockerTmpfs = [...]string{
	hermeticBuildPath + ":rw,noexec,nosuid,nodev,size=512m",
	hermeticRunPath + ":rw,exec,nosuid,nodev,size=64m",
	hermeticReplayPath + ":rw,noexec,nosuid,nodev,size=64m",
}

type FAC151DockerResult struct {
	ContainerID  string
	ExitCode     int
	OutputDigest string
	Removed      bool
}

type hermeticDockerRunner struct {
	sourceRoot       string
	candidateSHA     string
	policy           hermeticDockerPolicy
	docker           fixedDocker
	allowlist        func(string) error
	hostCache        func() (verifiedGoCache, error)
	archive          func(context.Context, string, string) ([]byte, string, error)
	operationTimeout time.Duration
}

type dockerResult struct {
	Output   []byte
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type dockerCommandError struct {
	Operation string
	ExitCode  int
	Stderr    string
	Cause     error
}

func (e *dockerCommandError) Error() string {
	return fmt.Sprintf("docker %s failed with exit status %d: %s", e.Operation, e.ExitCode, e.Stderr)
}

func (e *dockerCommandError) Unwrap() error { return e.Cause }

type dockerImageAbsentError struct {
	Reference string
	Cause     error
}

func (e *dockerImageAbsentError) Error() string {
	return fmt.Sprintf("Docker image is absent: %s", e.Reference)
}

func (e *dockerImageAbsentError) Unwrap() error { return e.Cause }

type dockerCommandFunc func(context.Context, []byte, []string) (dockerResult, error)

// RunFAC151Hermetic is the sole production entry point for the FAC-151
// native verifier profile. Its identity is derived from the current clean
// checkout; callers cannot provide a repository, task, image, command, key,
// receipt, replay root, or expected binding.
func RunFAC151Hermetic(ctx context.Context) (FAC151DockerResult, error) {
	root, err := fixedCheckoutRoot(ctx)
	if err != nil {
		return FAC151DockerResult{}, err
	}
	candidate, err := fixedGitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil || !validSHA(candidate) {
		return FAC151DockerResult{}, errors.New("FAC-151 requires an exact candidate HEAD")
	}
	if status, statusErr := fixedGitOutput(ctx, root, "status", "--porcelain", "--untracked-files=all"); statusErr != nil || status != "" {
		return FAC151DockerResult{}, errors.New("FAC-151 requires a clean checkout")
	}
	runner, err := newFAC151DockerRunner(root, candidate)
	if err != nil {
		return FAC151DockerResult{}, err
	}
	return runner.Run(ctx)
}

func fixedCheckoutRoot(ctx context.Context) (string, error) {
	working, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := fixedGitOutput(ctx, working, "rev-parse", "--show-toplevel")
	if err != nil || !filepath.IsAbs(root) {
		return "", errors.New("cannot resolve the current repository root")
	}
	return root, nil
}

func fixedGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", root}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	var output, diagnostic bytes.Buffer
	command.Stdout = &boundedBuffer{Buffer: &output, Limit: 64 << 10, label: "git stdout"}
	command.Stderr = &boundedBuffer{Buffer: &diagnostic, Limit: 64 << 10, label: "git stderr"}
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("fixed git query failed: %w: %s", err, diagnostic.String())
	}
	return strings.TrimSpace(output.String()), nil
}

type fixedDocker interface {
	Create(context.Context) (string, error)
	InspectImage(context.Context) error
	Pull(context.Context) error
	Inspect(context.Context, string) (dockerInspection, error)
	Start(context.Context, string) error
	Copy(context.Context, string, string, []byte) error
	Exec(context.Context, string, []string, []byte) (dockerResult, error)
	Remove(context.Context, string) error
}

func newFAC151DockerRunner(sourceRoot, candidateSHA string) (*hermeticDockerRunner, error) {
	if sourceRoot == "" || !filepath.IsAbs(sourceRoot) {
		return nil, errors.New("FAC-151 source root must be an absolute path")
	}
	if !validSHA(candidateSHA) {
		return nil, errors.New("FAC-151 candidate SHA is invalid")
	}
	return &hermeticDockerRunner{
		sourceRoot: sourceRoot, candidateSHA: candidateSHA, policy: fixedHermeticDockerPolicy(), docker: fixedDockerCLI{command: runDocker},
		allowlist: verifyFAC151Allowlist, hostCache: verifiedHostGoCache, archive: archiveCandidateSource,
	}, nil
}

func (r *hermeticDockerRunner) Run(ctx context.Context) (result FAC151DockerResult, err error) {
	if r == nil || r.docker == nil {
		return result, errors.New("FAC-151 Docker runner is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operationTimeout := r.operationTimeout
	if operationTimeout <= 0 {
		operationTimeout = hermeticTimeout
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	allowlist := r.allowlist
	if allowlist == nil {
		allowlist = verifyFAC151Allowlist
	}
	if err := allowlist(r.sourceRoot); err != nil {
		return result, err
	}
	hostCache := r.hostCache
	if hostCache == nil {
		hostCache = verifiedHostGoCache
	}
	cache, err := hostCache()
	if err != nil {
		return result, err
	}
	cacheTar, err := verifiedGoCacheTar(cache)
	if err != nil {
		return result, err
	}
	if err := ensureHermeticDockerImage(operationCtx, r.docker); err != nil {
		return result, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return result, fmt.Errorf("generate ephemeral launcher key: %w", err)
	}
	defer zeroPrivateKey(privateKey)
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return result, fmt.Errorf("generate independent receipt nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	containerID, err := r.docker.Create(operationCtx)
	if err != nil {
		return result, err
	}
	result.ContainerID = containerID
	removed := false
	defer func() {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		removeErr := r.docker.Remove(teardownCtx, containerID)
		_, inspectErr := r.docker.Inspect(teardownCtx, containerID)
		removed = isDockerContainerAbsent(inspectErr, containerID)
		var teardownErr error
		if removeErr != nil {
			teardownErr = errors.Join(teardownErr, removeErr)
		}
		if inspectErr != nil && !isDockerContainerAbsent(inspectErr, containerID) {
			teardownErr = errors.Join(teardownErr, inspectErr)
		} else if inspectErr == nil {
			teardownErr = errors.Join(teardownErr, errors.New("container still inspectable after teardown"))
		}
		result.Removed = removed
		if teardownErr != nil {
			err = errors.Join(err, teardownErr)
		}
	}()
	archive := r.archive
	if archive == nil {
		archive = archiveCandidateSource
	}
	sourceArchive, sourceDigest, err := archive(operationCtx, r.sourceRoot, r.candidateSHA)
	if err != nil {
		return result, err
	}
	transportArchive, err := rootCandidateArchiveForDocker(sourceArchive)
	if err != nil {
		return result, err
	}
	inspected, err := r.docker.Inspect(operationCtx, containerID)
	if err != nil {
		return result, err
	}
	if err := inspected.validate(r.policy, containerID, dockerInspectionPreStart); err != nil {
		return result, err
	}
	if err := r.docker.Start(operationCtx, containerID); err != nil {
		return result, fmt.Errorf("start fixed build profile: %w", err)
	}
	inspected, err = r.docker.Inspect(operationCtx, containerID)
	if err != nil {
		return result, err
	}
	if err := inspected.validate(r.policy, containerID, dockerInspectionPostStart); err != nil {
		return result, err
	}
	mountInfo, mountInfoErr := r.docker.Exec(operationCtx, containerID, []string{"/bin/cat", "/proc/self/mountinfo"}, nil)
	if mountInfoErr != nil {
		return result, errors.New("runtime_mountinfo_read_failed")
	}
	if err := validateRuntimeMountInfo(mountInfo.Output); err != nil {
		return result, err
	}
	if _, err := r.docker.Exec(operationCtx, containerID, []string{"/bin/mkdir", "-p", hermeticSourcePath}, nil); err != nil {
		return result, fmt.Errorf("prepare immutable source path: %w", err)
	}
	if _, err := r.docker.Exec(operationCtx, containerID, []string{"/bin/test", "-d", hermeticSourcePath}, nil); err != nil {
		return result, fmt.Errorf("prove immutable source directory: %w", err)
	}
	if err := r.docker.Copy(operationCtx, containerID, hermeticBuildPath, transportArchive); err != nil {
		return result, fmt.Errorf("copy immutable source archive: %w", err)
	}
	if _, err := r.docker.Exec(operationCtx, containerID, []string{"/bin/chmod", "a-w", hermeticSourcePath}, nil); err != nil {
		return result, fmt.Errorf("seal immutable source snapshot: %w", err)
	}
	if _, err := r.docker.Exec(operationCtx, containerID, []string{"/bin/mkdir", "-p", "/tmp/build/gomodcache/download/golang.org/x/sys/@v", "/tmp/build/gocache"}, nil); err != nil {
		return result, fmt.Errorf("prepare fixed container paths: %w", err)
	}
	if err := r.docker.Copy(operationCtx, containerID, "/tmp/build/gomodcache/download/golang.org/x/sys/@v", cacheTar); err != nil {
		return result, fmt.Errorf("copy verified Go cache closure: %w", err)
	}
	namespaces, uid, gid, err := r.namespaceIdentities(operationCtx, containerID)
	if err != nil {
		return result, err
	}
	argv := fixedFAC151Argv()
	if err := r.compile(operationCtx, containerID, publicKey, inspected.ID, namespaces, sourceDigest, argv, nonce); err != nil {
		return result, err
	}
	receipt := hermeticRunnerReceipt(r.candidateSHA, inspected.ID, namespaces, uid, gid, sourceDigest, argv, publicKey, nonce)
	if err := r.signAndCopyReceipt(operationCtx, containerID, receipt, privateKey); err != nil {
		return result, err
	}
	execution, execErr := r.docker.Exec(operationCtx, containerID, fixedFAC151Argv(), nil)
	result.ExitCode = execution.ExitCode
	sum := sha256.Sum256(execution.Output)
	result.OutputDigest = "sha256:" + hex.EncodeToString(sum[:])
	if execErr != nil || execution.ExitCode != 0 {
		if execErr != nil {
			return result, fmt.Errorf("FAC-151 verifier exited: %w", execErr)
		}
		return result, fmt.Errorf("FAC-151 verifier exited with status %d", execution.ExitCode)
	}
	return result, nil
}

type runtimeMountInfoRecord struct {
	root       string
	mountPoint string
	mountOpts  map[string]bool
	superOpts  map[string]bool
	fstype     string
	sizeBytes  int64
}

func validateRuntimeMountInfo(output []byte) error {
	if len(output) == 0 || len(output) > maxHermeticMountInfoBytes || output[len(output)-1] != '\n' {
		return errors.New("runtime_mountinfo_oversize")
	}
	lines := strings.Split(string(output[:len(output)-1]), "\n")
	fixed := map[string]bool{hermeticBuildPath: true, hermeticReplayPath: true, hermeticRunPath: true}
	seen := make(map[string]bool, len(fixed))
	for _, line := range lines {
		parts := strings.Split(line, " ")
		if len(parts) < 10 {
			return errors.New("runtime_mountinfo_malformed")
		}
		for _, field := range parts {
			if field == "" {
				return errors.New("runtime_mountinfo_malformed")
			}
			if _, err := decodeMountInfoField(field); err != nil {
				return errors.New("runtime_mountinfo_malformed")
			}
		}
		separator := -1
		for index := 6; index < len(parts); index++ {
			if parts[index] == "-" {
				if separator != -1 {
					return errors.New("runtime_mountinfo_malformed")
				}
				separator = index
			}
		}
		if separator < 6 || separator+3 >= len(parts) {
			return errors.New("runtime_mountinfo_malformed")
		}
		root, rootErr := decodeMountInfoField(parts[3])
		mountPoint, mountPointErr := decodeMountInfoField(parts[4])
		fstype, fstypeErr := decodeMountInfoField(parts[separator+1])
		mountOpts, mountOptsErr := decodeMountInfoField(parts[5])
		superOpts, superOptsErr := decodeMountInfoField(parts[separator+3])
		if rootErr != nil || mountPointErr != nil || fstypeErr != nil || mountOptsErr != nil || superOptsErr != nil {
			return errors.New("runtime_mountinfo_malformed")
		}
		mountOptions, mountOptionsErr := commaOptions(mountOpts)
		superOptions, superOptionsErr := commaOptions(superOpts)
		if mountOptionsErr != nil || superOptionsErr != nil {
			return errors.New("runtime_mountinfo_malformed")
		}
		record := runtimeMountInfoRecord{root: root, mountPoint: mountPoint, mountOpts: mountOptions, superOpts: superOptions, fstype: fstype}
		for destination := range fixed {
			if record.mountPoint != destination && !strings.HasPrefix(record.mountPoint, destination+"/") {
				continue
			}
			if record.mountPoint != destination {
				return fmt.Errorf("runtime_mountinfo_nested:%s", destination)
			}
			if seen[destination] {
				return fmt.Errorf("runtime_mountinfo_duplicate:%s", destination)
			}
			sizeBytes, sizeErr := parseTmpfsSizeBytes(superOpts)
			if sizeErr != nil {
				return errors.New("runtime_mountinfo_malformed")
			}
			record.sizeBytes = sizeBytes
			seen[destination] = true
			expectedSize := int64(64 << 20)
			if destination == hermeticBuildPath {
				expectedSize = 512 << 20
			}
			if record.root != "/" || record.fstype != "tmpfs" || record.sizeBytes != expectedSize || !record.mountOpts["rw"] || record.mountOpts["ro"] || record.superOpts["ro"] || !record.mountOpts["nosuid"] && !record.superOpts["nosuid"] || !record.mountOpts["nodev"] && !record.superOpts["nodev"] {
				return fmt.Errorf("runtime_mountinfo_wrong:%s", destination)
			}
			noexec := record.mountOpts["noexec"] || record.superOpts["noexec"]
			exec := record.mountOpts["exec"] || record.superOpts["exec"]
			if exec && noexec || (destination == hermeticBuildPath || destination == hermeticReplayPath) != noexec {
				return fmt.Errorf("runtime_mountinfo_wrong:%s", destination)
			}
		}
	}
	for destination := range fixed {
		if !seen[destination] {
			return fmt.Errorf("runtime_mountinfo_missing:%s", destination)
		}
	}
	return nil
}

func parseTmpfsSizeBytes(value string) (int64, error) {
	const maxInt64 = int64(1<<63 - 1)
	var parsed int64
	seen := false
	for _, option := range strings.Split(value, ",") {
		key := option
		if equal := strings.IndexByte(option, '='); equal >= 0 {
			key = option[:equal]
		}
		if strings.EqualFold(key, "size") {
			if seen || !strings.HasPrefix(option, "size=") {
				return 0, errors.New("invalid tmpfs size option")
			}
			seen = true
			raw := strings.TrimPrefix(option, "size=")
			if raw == "" {
				return 0, errors.New("invalid tmpfs size option")
			}
			multiplier := uint64(1)
			digits := raw
			switch raw[len(raw)-1] {
			case 'k':
				multiplier = 1 << 10
				digits = raw[:len(raw)-1]
			case 'm':
				multiplier = 1 << 20
				digits = raw[:len(raw)-1]
			case 'g':
				multiplier = 1 << 30
				digits = raw[:len(raw)-1]
			default:
				return 0, errors.New("invalid tmpfs size option")
			}
			if digits == "" {
				return 0, errors.New("invalid tmpfs size option")
			}
			for _, digit := range digits {
				if digit < '0' || digit > '9' {
					return 0, errors.New("invalid tmpfs size option")
				}
			}
			number, err := strconv.ParseUint(digits, 10, 63)
			if err != nil || number > uint64(maxInt64)/multiplier {
				return 0, errors.New("invalid tmpfs size option")
			}
			parsed = int64(number * multiplier)
		}
	}
	if !seen {
		return 0, errors.New("missing tmpfs size option")
	}
	return parsed, nil
}

func decodeMountInfoField(field string) (string, error) {
	if !strings.Contains(field, "\\") {
		return field, nil
	}
	var decoded strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] != '\\' {
			decoded.WriteByte(field[index])
			continue
		}
		if index+3 >= len(field) {
			return "", errors.New("invalid mountinfo escape")
		}
		switch field[index+1 : index+4] {
		case "040":
			decoded.WriteByte(' ')
		case "011":
			decoded.WriteByte('\t')
		case "012":
			decoded.WriteByte('\n')
		case "134":
			decoded.WriteByte('\\')
		default:
			return "", errors.New("invalid mountinfo escape")
		}
		index += 3
	}
	return decoded.String(), nil
}

func commaOptions(value string) (map[string]bool, error) {
	options := make(map[string]bool)
	for _, option := range strings.Split(value, ",") {
		if option == "" || options[option] {
			return nil, errors.New("invalid mountinfo option list")
		}
		options[option] = true
	}
	return options, nil
}

type fixedDockerCLI struct{ command dockerCommandFunc }

func ensureHermeticDockerImage(ctx context.Context, docker fixedDocker) error {
	err := docker.InspectImage(ctx)
	if err == nil {
		return nil
	}
	var absent *dockerImageAbsentError
	if !errors.As(err, &absent) || absent.Reference != hermeticDockerImage {
		return err
	}
	if err := docker.Pull(ctx); err != nil {
		return fmt.Errorf("pull fixed Docker image: %w", err)
	}
	return docker.InspectImage(ctx)
}

func (d fixedDockerCLI) run(ctx context.Context, input []byte, args []string) (dockerResult, error) {
	command := d.command
	if command == nil {
		command = runDocker
	}
	return command(ctx, input, args)
}

func (d fixedDockerCLI) Create(ctx context.Context) (string, error) {
	args := []string{"create", "--pull", "never", "--platform", hermeticDockerPlatform, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", hermeticContainerUser, "--pids-limit", strconv.Itoa(hermeticPIDLimit), "--memory", hermeticMemoryLimit}
	for _, tmpfs := range hermeticDockerTmpfs {
		args = append(args, "--tmpfs", tmpfs)
	}
	args = append(args, hermeticDockerImage, "/bin/sh", "-c", "exec sleep 3600")
	result, err := d.run(ctx, nil, args)
	if err != nil {
		return "", fmt.Errorf("create fixed FAC-151 container: %w", err)
	}
	id, err := parseDockerCreateRecord(result.Stdout)
	if err != nil {
		return "", err
	}
	return id, nil
}

func parseDockerCreateRecord(stdout []byte) (string, error) {
	const recordSize = 64 + 1
	if len(stdout) != recordSize || stdout[recordSize-1] != '\n' {
		return "", errors.New("Docker returned an invalid container ID record")
	}
	id := string(stdout[:recordSize-1])
	if !validDockerContainerID(id) {
		return "", errors.New("Docker returned an invalid container ID record")
	}
	return id, nil
}

func (d fixedDockerCLI) Inspect(ctx context.Context, id string) (dockerInspection, error) {
	result, err := d.run(ctx, nil, []string{"inspect", id})
	if err != nil {
		if isDockerContainerAbsent(err, id) {
			return dockerInspection{}, err
		}
		return dockerInspection{}, fmt.Errorf("inspect container before authority use: %w", err)
	}
	var values []dockerInspection
	if err := json.Unmarshal(result.Output, &values); err != nil || len(values) != 1 {
		return dockerInspection{}, errors.New("Docker inspect response is malformed")
	}
	return values[0], nil
}

func (d fixedDockerCLI) InspectImage(ctx context.Context) error {
	result, err := d.run(ctx, nil, []string{"image", "inspect", "--platform", hermeticDockerPlatform, hermeticDockerImage})
	if err != nil {
		if isDockerImageAbsent(err, hermeticDockerImage) {
			return &dockerImageAbsentError{Reference: hermeticDockerImage, Cause: err}
		}
		return fmt.Errorf("inspect fixed Docker image before create: %w", err)
	}
	var values []dockerImageInspection
	if err := json.Unmarshal(result.Output, &values); err != nil || len(values) != 1 {
		return errors.New("Docker image inspect response is malformed")
	}
	image := values[0]
	referenceDigest := pinnedDockerReferenceDigest()
	if referenceDigest == "" || (image.ID != hermeticDockerConfigDigest && image.ID != referenceDigest) || image.Architecture != "arm64" || image.OS != "linux" {
		return errors.New("Docker image manifest/config/platform evidence violates fixed policy")
	}
	seen := map[string]bool{}
	for _, value := range image.Config.Env {
		seen[value] = true
	}
	if !seen["GOLANG_VERSION=1.25.0"] || !seen["GOTOOLCHAIN=local"] {
		return errors.New("Docker image Go environment evidence violates fixed policy")
	}
	return nil
}

func pinnedDockerReferenceDigest() string {
	const prefix = "golang@sha256:"
	if !strings.HasPrefix(hermeticDockerImage, prefix) {
		return ""
	}
	digest := strings.TrimPrefix(hermeticDockerImage, "golang@")
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") || strings.ToLower(digest) != digest {
		return ""
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return ""
	}
	return digest
}

func (d fixedDockerCLI) Pull(ctx context.Context) error {
	_, err := d.run(ctx, nil, []string{"pull", "--platform", hermeticDockerPlatform, hermeticDockerImage})
	if err != nil {
		return fmt.Errorf("pull fixed Docker image: %w", err)
	}
	return nil
}

func (d fixedDockerCLI) Start(ctx context.Context, id string) error {
	_, err := d.run(ctx, nil, []string{"start", id})
	return err
}

func (d fixedDockerCLI) Copy(ctx context.Context, id, destination string, input []byte) error {
	_, err := d.run(ctx, input, []string{"cp", "-", id + ":" + destination})
	return err
}

func (d fixedDockerCLI) Exec(ctx context.Context, id string, argv []string, input []byte) (dockerResult, error) {
	args := append([]string{"exec", id}, argv...)
	return d.run(ctx, input, args)
}

func (d fixedDockerCLI) Remove(ctx context.Context, id string) error {
	_, err := d.run(ctx, nil, []string{"rm", "--force", id})
	return err
}

func (r *hermeticDockerRunner) namespaceIdentities(ctx context.Context, id string) (namespaceIdentity, int, int, error) {
	result, err := r.docker.Exec(ctx, id, []string{"/bin/sh", "-c", "stat -Lc '%i' /proc/1/ns/pid /proc/1/ns/user && id -u && id -g"}, nil)
	if err != nil {
		return namespaceIdentity{}, 0, 0, fmt.Errorf("read container namespace identities: %w", err)
	}
	parts := strings.Fields(string(result.Output))
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
		return namespaceIdentity{}, 0, 0, errors.New("container namespace identity response is malformed")
	}
	uid, uidErr := strconv.Atoi(parts[2])
	gid, gidErr := strconv.Atoi(parts[3])
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 || uid != 65532 || gid != 65532 {
		return namespaceIdentity{}, 0, 0, errors.New("container runtime UID/GID violates fixed policy")
	}
	return namespaceIdentity{PID: parts[0], User: parts[1]}, uid, gid, nil
}

func (r *hermeticDockerRunner) compile(ctx context.Context, id string, publicKey ed25519.PublicKey, containerID string, namespaces namespaceIdentity, sourceDigest string, argv []string, nonce string) error {
	ldflags := []string{
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151PublicKeyHex=" + hex.EncodeToString(publicKey),
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151CandidateSHA=" + r.candidateSHA,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151ContainerID=" + containerID,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151PIDNamespace=" + namespaces.PID,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151UserNamespace=" + namespaces.User,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151SourceDigest=" + sourceDigest,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151Nonce=" + nonce,
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151ArgvJSON=" + hex.EncodeToString([]byte(mustJSON(argv))),
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151Repository=github.com/Kampe/Herdforge",
		"-X", "github.com/Kampe/Herdforge/pkg/verifier.compiledFAC151Task=FAC-198/FAC-151",
	}
	args := []string{"/usr/bin/env", "GOTOOLCHAIN=local", "GOMODCACHE=/tmp/build/gomodcache", "GOCACHE=/tmp/build/gocache", "/usr/local/go/bin/go", "-C", hermeticSourcePath, "test", "-c", "-tags", "fac151_hermetic_integration", "-trimpath", "-count=" + hermeticTestCount, "-timeout", hermeticTestTimeout, "-ldflags", strings.Join(ldflags, " "), "-o", hermeticRunPath + "/verifier.test", "./pkg/verifier"}
	if _, err := r.docker.Exec(ctx, id, args, nil); err != nil {
		return fmt.Errorf("compile fixed FAC-151 verifier test binary: %w", err)
	}
	return nil
}

func (r *hermeticDockerRunner) signAndCopyReceipt(ctx context.Context, id string, receipt HermeticReceiptV1, privateKey ed25519.PrivateKey) error {
	receipt.Signature = ed25519.Sign(privateKey, signedPayload(receipt))
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	payload, err := boundedTarPayload([]tarPayload{{name: path.Base(hermeticReceiptPath), data: data, mode: 0o444}}, maxHermeticReceiptTarBytes)
	if err != nil {
		return fmt.Errorf("archive signed receipt: %w", err)
	}
	if err := r.docker.Copy(ctx, id, hermeticReplayPath, payload); err != nil {
		return fmt.Errorf("copy signed receipt into container tmpfs: %w", err)
	}
	return nil
}

func runDocker(ctx context.Context, input []byte, args []string) (dockerResult, error) {
	return runDockerWithProcess(ctx, input, args, executeDockerProcess)
}

func runDockerWithProcess(ctx context.Context, input []byte, args []string, process dockerCommandFunc) (dockerResult, error) {
	result, err := process(ctx, input, args)
	if err != nil {
		return result, &dockerCommandError{Operation: dockerOperation(args), ExitCode: result.ExitCode, Stderr: string(result.Stderr), Cause: err}
	}
	return result, nil
}

func executeDockerProcess(ctx context.Context, input []byte, args []string) (dockerResult, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &boundedBuffer{Buffer: &stdout, Limit: 4 << 20, label: "Docker stdout"}
	command.Stderr = &boundedBuffer{Buffer: &stderr, Limit: 64 << 10, label: "Docker stderr"}
	err := command.Run()
	output := append(stdout.Bytes(), stderr.Bytes()...)
	result := dockerResult{Output: output, Stdout: append([]byte(nil), stdout.Bytes()...), Stderr: append([]byte(nil), stderr.Bytes()...), ExitCode: 0}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	return result, err
}

type dockerInspection struct {
	ID     string `json:"Id"`
	Config struct {
		Image string `json:"Image"`
		User  string `json:"User"`
	} `json:"Config"`
	HostConfig struct {
		Binds          []string          `json:"Binds"`
		NetworkMode    string            `json:"NetworkMode"`
		PidMode        string            `json:"PidMode"`
		UsernsMode     string            `json:"UsernsMode"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		PidsLimit      int64             `json:"PidsLimit"`
		Memory         int64             `json:"Memory"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

type dockerInspectionStage uint8

const (
	dockerInspectionPreStart dockerInspectionStage = iota
	dockerInspectionPostStart
)

const maxDockerMountDiagnosticBytes = 2048

func dockerMountShapeDiagnostic(i dockerInspection, stage dockerInspectionStage, allowed map[string]bool) error {
	stageName := "pre_start"
	if stage == dockerInspectionPostStart {
		stageName = "post_start"
	}
	fixedDestinations := []string{hermeticBuildPath, hermeticReplayPath, hermeticRunPath}
	sort.Strings(fixedDestinations)
	hostEntries := make([]string, 0, len(fixedDestinations))
	for _, destination := range fixedDestinations {
		options, ok := i.HostConfig.Tmpfs[destination]
		if !ok {
			options = "<missing>"
		}
		hostEntries = append(hostEntries, destination+"="+options)
	}
	mountEntries := make([]string, 0, len(i.Mounts))
	for _, mount := range i.Mounts {
		destination := mount.Destination
		if !allowed[destination] {
			destination = "<non-fixed>"
		}
		typeName := mount.Type
		if len(typeName) > 32 {
			typeName = typeName[:32] + "..."
		}
		mountEntries = append(mountEntries, fmt.Sprintf("(%s,%s,%t)", typeName, destination, mount.RW))
	}
	sort.Strings(mountEntries)
	diagnostic := fmt.Sprintf("Docker mounts do not match fixed tmpfs-only policy: stage=%s host_tmpfs_count=%d host_tmpfs=[%s] mounts_count=%d mounts=[%s]", stageName, len(i.HostConfig.Tmpfs), strings.Join(hostEntries, ","), len(i.Mounts), strings.Join(mountEntries, ","))
	if len(diagnostic) > maxDockerMountDiagnosticBytes {
		diagnostic = diagnostic[:maxDockerMountDiagnosticBytes-len("...")] + "..."
	}
	return errors.New(diagnostic)
}

type dockerImageInspection struct {
	ID           string `json:"Id"`
	Architecture string `json:"Architecture"`
	OS           string `json:"Os"`
	Config       struct {
		Env []string `json:"Env"`
	} `json:"Config"`
}

func (i dockerInspection) validate(policy hermeticDockerPolicy, expectedContainerID string, stage dockerInspectionStage) error {
	if expectedContainerID == "" || i.ID != expectedContainerID || i.Config.Image != policy.image() || i.Config.User != hermeticContainerUser || i.HostConfig.NetworkMode != "none" || i.HostConfig.PidMode != "" || i.HostConfig.UsernsMode != "" || len(i.HostConfig.Binds) != 0 || !i.HostConfig.ReadonlyRootfs || i.HostConfig.PidsLimit != hermeticPIDLimit || i.HostConfig.Memory != hermeticMemoryBytes {
		return errors.New("Docker container inspection violates fixed hermetic policy")
	}
	if len(i.HostConfig.CapDrop) != 1 || i.HostConfig.CapDrop[0] != "ALL" || !containsString(i.HostConfig.SecurityOpt, "no-new-privileges:true") {
		return errors.New("Docker capability or privilege inspection violates fixed hermetic policy")
	}
	allowed := map[string]bool{hermeticBuildPath: true, hermeticRunPath: true, hermeticReplayPath: true}
	if stage != dockerInspectionPreStart && stage != dockerInspectionPostStart {
		return errors.New("Docker inspection stage is invalid")
	}
	if len(i.HostConfig.Tmpfs) != len(allowed) || (len(i.Mounts) != 0 && len(i.Mounts) != len(allowed)) {
		return dockerMountShapeDiagnostic(i, stage, allowed)
	}
	for destination, options := range map[string]string{hermeticBuildPath: "rw,noexec,nosuid,nodev,size=512m", hermeticRunPath: "rw,exec,nosuid,nodev,size=64m", hermeticReplayPath: "rw,noexec,nosuid,nodev,size=64m"} {
		if got, ok := i.HostConfig.Tmpfs[destination]; !ok || got != options {
			return errors.New("Docker tmpfs map does not match fixed policy")
		}
	}
	seenMounts := make(map[string]struct{}, len(i.Mounts))
	for _, mount := range i.Mounts {
		if mount.Type != "tmpfs" || !allowed[mount.Destination] || !mount.RW {
			return errors.New("Docker mount violates fixed tmpfs-only policy")
		}
		if _, exists := seenMounts[mount.Destination]; exists {
			return errors.New("Docker mount contains a duplicate destination")
		}
		seenMounts[mount.Destination] = struct{}{}
	}
	if len(i.Mounts) != 0 && len(seenMounts) != len(allowed) {
		return errors.New("Docker mount realization is incomplete")
	}
	return nil
}

type namespaceIdentity struct{ PID, User string }

func fixedFAC151Argv() []string {
	return []string{hermeticRunPath + "/verifier.test", "-test.run", fixedFAC151Regex(), "-test.count=" + hermeticTestCount, "-test.timeout=" + hermeticTestTimeout}
}

func fixedFAC151Regex() string { return "^(?:" + strings.Join(hermeticFAC151Allowlist[:], "|") + ")$" }

func hermeticRunnerReceipt(candidateSHA string, containerID string, namespaces namespaceIdentity, uid, gid int, sourceDigest string, argv []string, publicKey ed25519.PublicKey, nonce string) HermeticReceiptV1 {
	now := time.Now().UTC()
	keyVerifier, _ := NewTrustedReceiptVerifier(publicKey)
	receipt := HermeticReceiptV1{Version: HermeticReceiptVersion, Repository: "github.com/Kampe/Herdforge", Task: "FAC-198/FAC-151", CandidateSHA: candidateSHA, Argv: argv, ArgvDigest: digestArgv(argv), Isolation: IsolationBinding{Kind: IsolationContainer, ContainerIdentity: containerID}, PIDNamespaceIdentity: namespaces.PID, UserNamespaceIdentity: namespaces.User, UID: uid, GID: gid, NetworkMode: "none", MountPolicy: "immutable-copy-no-host-bind", SourceCopyDigest: sourceDigest, StartedAt: now, ExpiresAt: now.Add(HermeticReceiptMaxTTL), Generation: keyVerifier.authorityKeyID(), Nonce: nonce}
	receipt.PayloadDigest = payloadDigest(receipt)
	return receipt
}

func mustJSON(value any) string { data, _ := json.Marshal(value); return string(data) }

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func archiveCandidateSource(ctx context.Context, root, candidateSHA string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, "git", fixedGitArchiveArgs(root, candidateSHA)...)
	var output, diagnostic bytes.Buffer
	command.Stdout = &boundedBuffer{Buffer: &output, Limit: maxHermeticSourceArchiveBytes, label: "candidate archive"}
	command.Stderr = &boundedBuffer{Buffer: &diagnostic, Limit: 64 << 10, label: "git stderr"}
	if err := command.Run(); err != nil {
		return nil, "", fmt.Errorf("archive exact candidate tree: %w: %s", err, diagnostic.String())
	}
	if output.Len() == 0 {
		return nil, "", errors.New("candidate archive is empty")
	}
	digest, err := sourceManifestDigestFromArchive(output.Bytes())
	if err != nil {
		return nil, "", err
	}
	return output.Bytes(), digest, nil
}

func rootCandidateArchiveForDocker(archiveBytes []byte) ([]byte, error) {
	if _, err := sourceManifestDigestFromArchive(archiveBytes); err != nil {
		return nil, err
	}
	reader := tar.NewReader(bytes.NewReader(archiveBytes))
	var output bytes.Buffer
	bounded := &boundedBuffer{Buffer: &output, Limit: maxHermeticSourceTransportBytes, label: "source transport archive"}
	writer := tar.NewWriter(bounded)
	if err := writer.WriteHeader(&tar.Header{Name: hermeticSourceTransportRoot + "/", Mode: 0o555, Uid: 0, Gid: 0, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	memberCount := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("read candidate transport archive")
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := validateArchiveMemberMetadata(header); err != nil {
			return nil, err
		}
		rawName := header.Name
		if header.Typeflag == tar.TypeDir {
			rawName = strings.TrimSuffix(rawName, "/")
		}
		name, err := validateManifestName(rawName)
		if err != nil {
			return nil, err
		}
		if name == hermeticSourceTransportRoot || strings.HasPrefix(name, hermeticSourceTransportRoot+"/") {
			return nil, errors.New("candidate transport archive contains a duplicate source root")
		}
		if len(name) > maxHermeticSourceTransportPathBytes || len(header.Linkname) > maxHermeticSourceTransportPathBytes {
			return nil, errors.New("candidate transport archive path exceeds bounds")
		}
		if memberCount >= maxHermeticSourceTransportMembers {
			return nil, errors.New("candidate transport archive exceeds member bounds")
		}
		transportHeader := *header
		transportHeader.Name = hermeticSourceTransportRoot + "/" + name
		transportHeader.PAXRecords = nil
		transportHeader.Xattrs = nil
		transportHeader.Uname = ""
		transportHeader.Gname = ""
		transportHeader.AccessTime = time.Time{}
		transportHeader.ChangeTime = time.Time{}
		transportHeader.ModTime = time.Time{}
		transportHeader.Uid = 0
		transportHeader.Gid = 0
		transportHeader.Mode &= 0o777
		if header.Typeflag == tar.TypeDir {
			transportHeader.Name += "/"
		}
		if header.Typeflag == tar.TypeSymlink {
			if err := validateSymlinkTarget(transportHeader.Name, header.Linkname); err != nil {
				return nil, err
			}
			transportHeader.Linkname = header.Linkname
		} else {
			transportHeader.Linkname = ""
		}
		if err := writer.WriteHeader(&transportHeader); err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			count, copyErr := io.CopyN(writer, reader, header.Size)
			if copyErr != nil || count != header.Size {
				return nil, errors.New("candidate transport archive member is truncated")
			}
		}
		memberCount++
	}
	if memberCount == 0 {
		return nil, errors.New("candidate transport archive has no source members")
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if int64(output.Len()) > maxHermeticSourceTransportBytes {
		return nil, errors.New("candidate transport archive exceeds bounds")
	}
	return output.Bytes(), nil
}

func fixedGitArchiveArgs(root, candidateSHA string) []string {
	return []string{"-c", "tar.umask=0022", "-C", root, "archive", "--format=tar", candidateSHA}
}

const (
	maxSourceManifestFileBytes          = 16 << 20
	maxSourceManifestTotalBytes         = 16 << 20
	maxHermeticSourceTransportMembers   = 16384
	maxHermeticSourceTransportPathBytes = 4096
	maxHermeticSourceTransportBytes     = maxSourceManifestTotalBytes + int64(maxHermeticSourceTransportMembers+1)*(1024+2*maxHermeticSourceTransportPathBytes)
)

type sourceManifestRecord struct {
	name   string
	kind   byte
	mode   int64
	size   int64
	digest string
}

func sourceManifestDigestFromArchive(archiveBytes []byte) (string, error) {
	reader := tar.NewReader(bytes.NewReader(archiveBytes))
	records := make([]sourceManifestRecord, 0)
	seen := make(map[string]struct{})
	var total int64
	metadataSeen := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read candidate archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if metadataSeen {
				return "", errors.New("candidate archive contains duplicate global metadata")
			}
			if err := validateArchiveGlobalMetadata(header); err != nil {
				return "", err
			}
			metadataSeen = true
			continue
		}
		if header.Name == "pax_global_header" {
			return "", errors.New("candidate archive reserves global metadata path")
		}
		if err := validateArchiveMemberMetadata(header); err != nil {
			return "", err
		}
		rawName := header.Name
		if header.Typeflag == tar.TypeDir {
			rawName = strings.TrimSuffix(rawName, "/")
		}
		name, err := validateManifestName(rawName)
		if err != nil {
			return "", err
		}
		if _, exists := seen[name]; exists {
			return "", errors.New("candidate archive contains duplicate manifest path")
		}
		seen[name] = struct{}{}
		mode := int64(header.Mode & 0111)
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxSourceManifestFileBytes || total+header.Size > maxSourceManifestTotalBytes {
				return "", errors.New("candidate archive exceeds source manifest bounds")
			}
			hash := sha256.New()
			count, copyErr := io.CopyN(hash, reader, header.Size)
			if copyErr != nil || count != header.Size {
				return "", errors.New("candidate archive regular file is truncated")
			}
			total += count
			records = append(records, sourceManifestRecord{name: name, kind: 'f', mode: mode, size: count, digest: hex.EncodeToString(hash.Sum(nil))})
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(name, header.Linkname); err != nil {
				return "", err
			}
			target := []byte(header.Linkname)
			if len(target) > maxSourceManifestFileBytes || total+int64(len(target)) > maxSourceManifestTotalBytes {
				return "", errors.New("candidate archive symlink exceeds source manifest bounds")
			}
			sum := sha256.Sum256(target)
			total += int64(len(target))
			records = append(records, sourceManifestRecord{name: name, kind: 'l', mode: mode, size: int64(len(target)), digest: hex.EncodeToString(sum[:])})
		default:
			return "", errors.New("candidate archive contains unsupported file type")
		}
	}
	return sourceManifestDigestFromRecords(records)
}

func sourceManifestDigestFromFilesystem(root string) (string, error) {
	return sourceManifestDigestFromFilesystemWithReader(root, collectFilesystemReadbackMembers)
}

type filesystemReadbackReader func(string) ([]filesystemManifestMember, error)

func sourceManifestDigestFromFilesystemWithReader(root string, readMembers filesystemReadbackReader) (string, error) {
	members, err := readMembers(root)
	if err != nil {
		return "", err
	}
	return sourceManifestDigestFromFilesystemMembers(root, members)
}

type filesystemManifestMember struct {
	name   string
	info   os.FileInfo
	target string
}

func collectFilesystemReadbackMembers(root string) ([]filesystemManifestMember, error) {
	members := make([]filesystemManifestMember, 0)
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == root {
			return nil
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		clean, err := validateManifestName(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		member := filesystemManifestMember{name: clean, info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			member.target, err = os.Readlink(name)
			if err != nil {
				return err
			}
		}
		members = append(members, member)
		return nil
	})
	return members, err
}

func sourceManifestDigestFromFilesystemMembers(root string, members []filesystemManifestMember) (string, error) {
	records := make([]sourceManifestRecord, 0)
	seen := make(map[string]struct{})
	var total int64
	for _, member := range members {
		clean := member.name
		if _, exists := seen[clean]; exists {
			return "", errors.New("copied source contains duplicate manifest path")
		}
		seen[clean] = struct{}{}
		info := member.info
		if err := validateFilesystemMemberMetadata(info); err != nil {
			return "", fmt.Errorf("copied source member %q: %w", clean, err)
		}
		mode := int64(info.Mode().Perm() & 0111)
		if info.IsDir() {
			continue
		}
		switch {
		case info.Mode().IsRegular():
			if info.Size() < 0 || info.Size() > maxSourceManifestFileBytes || total+info.Size() > maxSourceManifestTotalBytes {
				return "", errors.New("copied source exceeds source manifest bounds")
			}
			file, err := os.Open(filepath.Join(root, filepath.FromSlash(clean)))
			if err != nil {
				return "", err
			}
			hash := sha256.New()
			count, copyErr := io.Copy(hash, io.LimitReader(file, maxSourceManifestFileBytes+1))
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil || count != info.Size() {
				return "", errors.New("copied source regular file is truncated or changed")
			}
			total += count
			records = append(records, sourceManifestRecord{name: clean, kind: 'f', mode: mode, size: count, digest: hex.EncodeToString(hash.Sum(nil))})
		case info.Mode()&os.ModeSymlink != 0:
			if err := validateSymlinkTarget(clean, member.target); err != nil {
				return "", err
			}
			if len(member.target) > maxSourceManifestFileBytes || total+int64(len(member.target)) > maxSourceManifestTotalBytes {
				return "", errors.New("copied source symlink exceeds source manifest bounds")
			}
			sum := sha256.Sum256([]byte(member.target))
			total += int64(len(member.target))
			records = append(records, sourceManifestRecord{name: clean, kind: 'l', mode: mode, size: int64(len(member.target)), digest: hex.EncodeToString(sum[:])})
		default:
			return "", errors.New("copied source contains unsupported file type")
		}
	}
	return sourceManifestDigestFromRecords(records)
}

func sourceManifestDigestFromRecords(records []sourceManifestRecord) (string, error) {
	if len(records) == 0 {
		return "", errors.New("source manifest is empty")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].name < records[j].name })
	var manifest strings.Builder
	for _, record := range records {
		fmt.Fprintf(&manifest, "%c|%o|%d|%s|%s\n", record.kind, record.mode, record.size, record.name, record.digest)
	}
	sum := sha256.Sum256([]byte(manifest.String()))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateManifestName(name string) (string, error) {
	clean := path.Clean(name)
	if name == "" || name != clean || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(name, "\x00\n\\") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" || (len(name) > 1 && name[1] == ':') {
		return "", errors.New("source manifest contains unsafe path")
	}
	return clean, nil
}

func validateArchiveMemberMetadata(header *tar.Header) error {
	if header == nil || header.Uid != 0 || header.Gid != 0 {
		return errors.New("candidate archive member is not root-owned")
	}
	if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
		mode := header.Mode & 0o777
		if mode != 0o644 && mode != 0o755 {
			return errors.New("candidate archive regular file has an unsafe Git mode")
		}
	} else if header.Typeflag == tar.TypeDir && header.Mode&0002 != 0 {
		return errors.New("candidate archive directory is world-writable")
	}
	return nil
}

func validateArchiveGlobalMetadata(header *tar.Header) error {
	if header.Name != "pax_global_header" || header.Linkname != "" || header.Uid != 0 || header.Gid != 0 || header.Mode != 0 || header.Size != 0 {
		return errors.New("candidate archive global metadata has unsafe metadata")
	}
	if len(header.PAXRecords) != 1 {
		return errors.New("candidate archive global metadata is ambiguous")
	}
	commit, ok := header.PAXRecords["comment"]
	if !ok || len(commit) != 40 || strings.ToLower(commit) != commit || !validSHA(commit) {
		return errors.New("candidate archive global metadata has an invalid commit record")
	}
	return nil
}

func validateFilesystemMemberMetadata(info os.FileInfo) error {
	uid, gid, ok := filesystemOwner(info)
	if !ok || uid != 0 || gid != 0 {
		return errors.New("copied source member is not root-owned")
	}
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		permissions := mode.Perm()
		if permissions != 0o644 && permissions != 0o755 {
			return errors.New("copied source regular file has an unsafe Git mode")
		}
	case mode.IsDir():
		if mode.Perm()&0002 != 0 {
			return errors.New("copied source directory is world-writable")
		}
	case mode&os.ModeSymlink != 0:
		// Symlink permission bits are not meaningful; containment is checked below.
	default:
		return errors.New("copied source member has an unsupported type")
	}
	return nil
}

func validateSymlinkTarget(name, target string) error {
	if target == "" || path.IsAbs(target) || strings.ContainsAny(target, "\\\x00\n") {
		return fmt.Errorf("symlink %q has an unsafe target", name)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink %q escapes source root", name)
	}
	return nil
}

type boundedBuffer struct {
	*bytes.Buffer
	Limit int64
	label string
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.Limit {
		label := b.label
		if label == "" {
			label = "bounded output"
		}
		return 0, fmt.Errorf("%s exceeds limit", label)
	}
	return b.Buffer.Write(p)
}

type tarPayload struct {
	name string
	data []byte
	mode int64
}

func boundedTarPayload(files []tarPayload, limit int64) ([]byte, error) {
	var output bytes.Buffer
	bounded := &boundedBuffer{Buffer: &output, Limit: limit, label: "tar payload"}
	writer := tar.NewWriter(bounded)
	for _, file := range files {
		if err := validateManifestNameForTar(file.name); err != nil {
			return nil, err
		}
		if len(file.data) == 0 {
			return nil, errors.New("tar payload file is empty")
		}
		mode := file.mode
		if mode == 0 {
			mode = 0o444
		}
		if mode&0222 != 0 {
			return nil, errors.New("tar payload member is writable")
		}
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: mode, Size: int64(len(file.data))}); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if int64(output.Len()) > limit {
		return nil, errors.New("tar payload exceeds limit")
	}
	if err := validateTarPayload(output.Bytes(), files); err != nil {
		return nil, fmt.Errorf("tar payload is invalid: %w", err)
	}
	return output.Bytes(), nil
}

func validateTarPayload(payload []byte, expected []tarPayload) error {
	reader := tar.NewReader(bytes.NewReader(payload))
	for index, want := range expected {
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("read member %d: %w", index, err)
		}
		if err := validateManifestNameForTar(header.Name); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Name != want.name || header.Uid != 0 || header.Gid != 0 || header.Mode&0o777 != 0o444 || header.Size != int64(len(want.data)) {
			return fmt.Errorf("tar payload member %d metadata does not match exact 0444 contract", index)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, want.data) {
			return fmt.Errorf("tar payload member %d content does not match expected bytes", index)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return errors.New("tar payload contains an unexpected extra member")
	}
	return nil
}

func validateManifestNameForTar(name string) error {
	if _, err := validateManifestName(name); err != nil {
		return fmt.Errorf("tar payload path: %w", err)
	}
	return nil
}

func zeroPrivateKey(key ed25519.PrivateKey) {
	for index := range key {
		key[index] = 0
	}
}

func validDockerContainerID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func dockerOperation(args []string) string {
	if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
		return "image inspect"
	}
	if len(args) == 0 {
		return "unknown"
	}
	return args[0]
}

func isDockerContainerAbsent(err error, id string) bool {
	if err == nil || !validDockerContainerID(id) {
		return false
	}
	var commandErr *dockerCommandError
	if !errors.As(err, &commandErr) || commandErr.Operation != "inspect" || commandErr.ExitCode != 1 {
		return false
	}
	stderr := strings.TrimSpace(commandErr.Stderr)
	return stderr == "Error response from daemon: No such container: "+id || stderr == "Error: No such object: "+id
}

func isDockerImageAbsent(err error, reference string) bool {
	if err == nil || reference == "" {
		return false
	}
	var commandErr *dockerCommandError
	if !errors.As(err, &commandErr) || commandErr.Operation != "image inspect" || commandErr.ExitCode != 1 {
		return false
	}
	return strings.TrimSpace(commandErr.Stderr) == "Error response from daemon: No such image: "+reference
}
