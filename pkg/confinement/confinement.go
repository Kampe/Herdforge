// Package confinement defines the foundation boundary used by future worker
// enforcement. It deliberately has no production call sites yet.
package confinement

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrUnauthenticated     = errors.New("confinement: unauthenticated capability")
	ErrOutsideRoot         = errors.New("confinement: path is outside authenticated worktree")
	ErrSymlink             = errors.New("confinement: symlink path component is not allowed")
	ErrCaseAlias           = errors.New("confinement: case-alias path is not allowed")
	ErrInvalidSentinel     = errors.New("confinement: invalid or missing worktree sentinel")
	ErrHardlink            = errors.New("confinement: hardlink path is not allowed")
	ErrDifferentDevice     = errors.New("confinement: path is on a different device")
	ErrUnsupportedIdentity = errors.New("confinement: unsupported filesystem identity")
	ErrInvalidCommand      = errors.New("confinement: invalid or unbounded command")
	ErrSentinelMutation    = errors.New("confinement: sentinel mutation is not allowed")
)

const sentinelContents = "herdforge-worktree-sentinel\n"

// Capability is intentionally opaque to callers. A root string or sentinel
// path alone cannot be used as authorization.
type Capability struct {
	root       string
	sentinel   string
	tuple      AuthTuple
	proof      IssuerProof
	binding    [32]byte
	rootID     fileIdentity
	sentinelID fileIdentity
}

// AuthTuple is the identity and policy context bound to a capability.
type AuthTuple struct {
	Repository        string
	Task              string
	LeaseID           string
	Lane              string
	Session           string
	SessionGeneration string
	HerdrTab          string
	HerdrPane         string
	ProcessIdentity   string
	ArgvIdentity      string
	PolicyDigest      string
	AllowedRoots      []string
}

// Issuer is the missing production authority seam. A real implementation must
// verify a MAC/signature and nonce issued for the complete AuthTuple.
type Issuer interface {
	Issue(root, sentinel string, tuple AuthTuple) (IssuerProof, error)
}

type IssuerProof struct {
	MAC   []byte
	Nonce string
}

// Boundary is a fail-closed policy-planning seam. It does not launch processes,
// install a sandbox, or make an already-authorized write atomic.
type Boundary interface {
	AuthorizeWrite(Capability, string) error
	AuthorizeCommand(Capability, Command) error
}

type boundary struct{}

// New authenticates a fixture worktree using a separate sentinel file.
func New(root, sentinel string, tuple AuthTuple, issuer Issuer) (Boundary, Capability, error) {
	if issuer == nil || tuple.Repository == "" || tuple.Task == "" || tuple.LeaseID == "" || tuple.Lane == "" || tuple.Session == "" || tuple.SessionGeneration == "" || tuple.HerdrTab == "" || tuple.HerdrPane == "" || tuple.ProcessIdentity == "" || tuple.ArgvIdentity == "" || tuple.PolicyDigest == "" || len(tuple.AllowedRoots) == 0 {
		return nil, Capability{}, ErrUnauthenticated
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, Capability{}, fmt.Errorf("authenticate root: %w", err)
	}
	rootCanonical, err := canonicalRoot(root)
	if err != nil {
		return nil, Capability{}, fmt.Errorf("authenticate root: %w", err)
	}
	info, err := os.Stat(rootCanonical)
	if err != nil || !info.IsDir() {
		return nil, Capability{}, ErrInvalidSentinel
	}
	rootID, err := identityOfInfo(info)
	if err != nil {
		return nil, Capability{}, err
	}
	canonicalRoots := make([]string, 0, len(tuple.AllowedRoots))
	rootAllowed := false
	for _, allowedRoot := range tuple.AllowedRoots {
		canonicalAllowed, err := canonicalRoot(allowedRoot)
		if err != nil {
			return nil, Capability{}, ErrUnauthenticated
		}
		canonicalRoots = append(canonicalRoots, canonicalAllowed)
		rootAllowed = rootAllowed || canonicalAllowed == rootCanonical
	}
	if !rootAllowed {
		return nil, Capability{}, ErrUnauthenticated
	}
	tuple.AllowedRoots = canonicalRoots
	sentinelAbs, err := filepath.Abs(filepath.Clean(sentinel))
	if err != nil {
		return nil, Capability{}, fmt.Errorf("authenticate sentinel: %w", ErrInvalidSentinel)
	}
	relativeSentinel, err := filepath.Rel(rootAbs, sentinelAbs)
	if err != nil {
		return nil, Capability{}, fmt.Errorf("authenticate sentinel: %w", ErrInvalidSentinel)
	}
	sentinelCanonical, err := canonicalExisting(filepath.Join(rootCanonical, relativeSentinel))
	if err != nil {
		return nil, Capability{}, fmt.Errorf("authenticate sentinel: %w", ErrInvalidSentinel)
	}
	if !isDescendant(sentinelCanonical, rootCanonical) {
		return nil, Capability{}, ErrInvalidSentinel
	}
	data, err := os.ReadFile(sentinelCanonical)
	if err != nil || string(data) != sentinelContents {
		return nil, Capability{}, ErrInvalidSentinel
	}
	sentinelInfo, err := os.Lstat(sentinelCanonical)
	if err != nil {
		return nil, Capability{}, ErrInvalidSentinel
	}
	sentinelID, err := identityOfInfo(sentinelInfo)
	if err != nil {
		return nil, Capability{}, err
	}
	proof, err := issuer.Issue(rootCanonical, sentinelCanonical, tuple)
	if err != nil || len(proof.MAC) == 0 || proof.Nonce == "" {
		return nil, Capability{}, ErrUnauthenticated
	}
	cap := Capability{root: rootCanonical, sentinel: sentinelCanonical, tuple: tuple, proof: proof, rootID: rootID, sentinelID: sentinelID}
	cap.binding = cap.computeBinding()
	return boundary{}, cap, nil
}

func canonicalRoot(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", ErrSymlink
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return canonicalExisting(filepath.Join(parent, filepath.Base(abs)))
}

// Command describes one process launch and the filesystem paths it may touch.
// Children are checked recursively, which gives future process supervisors a
// single propagation seam without claiming enforcement today.
type Command struct {
	Name            string
	ProcessIdentity string
	ArgvIdentity    string
	Paths           []string
	Children        []Command
}

const (
	maxCommandDepth = 8
	maxCommandNodes = 64
)

func (boundary) AuthorizeWrite(cap Capability, path string) error {
	if err := validateCapability(cap); err != nil {
		return err
	}
	canonical, err := canonicalForWrite(path, cap.root)
	if err != nil {
		return err
	}
	if !isDescendant(canonical, cap.root) {
		return ErrOutsideRoot
	}
	if canonical == cap.sentinel {
		return ErrSentinelMutation
	}
	if err := sameDevice(canonical, cap.rootID); err != nil {
		return err
	}
	return nil
}

func (b boundary) AuthorizeCommand(cap Capability, command Command) error {
	if err := validateCapability(cap); err != nil {
		return err
	}
	nodes := 0
	return b.authorizeCommand(cap, command, 0, &nodes, true)
}

func (b boundary) authorizeCommand(cap Capability, command Command, depth int, nodes *int, root bool) error {
	if command.Name == "" || command.ProcessIdentity == "" || command.ArgvIdentity == "" || len(command.Paths)+len(command.Children) == 0 || depth >= maxCommandDepth || *nodes >= maxCommandNodes || *nodes+1+len(command.Paths) > maxCommandNodes {
		return ErrInvalidCommand
	}
	if root && (command.ProcessIdentity != cap.tuple.ProcessIdentity || command.ArgvIdentity != cap.tuple.ArgvIdentity) {
		return ErrInvalidCommand
	}
	*nodes = *nodes + 1
	*nodes += len(command.Paths)
	for _, path := range command.Paths {
		if err := b.AuthorizeWrite(cap, path); err != nil {
			return fmt.Errorf("command %q: %w", command.Name, err)
		}
	}
	for _, child := range command.Children {
		if err := b.authorizeCommand(cap, child, depth+1, nodes, false); err != nil {
			return fmt.Errorf("child of %q: %w", command.Name, err)
		}
	}
	return nil
}

func validateCapability(cap Capability) error {
	if cap.root == "" || cap.sentinel == "" || !isDescendant(cap.sentinel, cap.root) || len(cap.proof.MAC) == 0 || cap.proof.Nonce == "" || cap.binding != cap.computeBinding() {
		return ErrUnauthenticated
	}
	canonicalSentinel, err := canonicalExisting(cap.sentinel)
	if err != nil || canonicalSentinel != cap.sentinel {
		return ErrInvalidSentinel
	}
	currentRootID, err := identityOfPath(cap.root)
	if err != nil || currentRootID != cap.rootID {
		return ErrInvalidSentinel
	}
	currentSentinelID, err := identityOfPath(cap.sentinel)
	if err != nil || currentSentinelID != cap.sentinelID {
		return ErrInvalidSentinel
	}
	data, err := os.ReadFile(cap.sentinel)
	if err != nil || string(data) != sentinelContents {
		return ErrInvalidSentinel
	}
	return nil
}

func (cap Capability) computeBinding() [32]byte {
	parts := []string{cap.root, cap.sentinel, string(cap.proof.MAC), cap.proof.Nonce, cap.tuple.Repository, cap.tuple.Task, cap.tuple.LeaseID, cap.tuple.Lane, cap.tuple.Session, cap.tuple.SessionGeneration, cap.tuple.HerdrTab, cap.tuple.HerdrPane, cap.tuple.ProcessIdentity, cap.tuple.ArgvIdentity, cap.tuple.PolicyDigest}
	parts = append(parts, cap.tuple.AllowedRoots...)
	return sha256.Sum256([]byte(strings.Join(parts, "\x00")))
}

func canonicalForWrite(path, root string) (string, error) {
	if path == "" {
		return "", ErrOutsideRoot
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return "", ErrOutsideRoot
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return canonicalExisting(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	probe := path
	var missing []string
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", os.ErrNotExist
		}
		probe = parent
	}
	parent, err := canonicalExisting(probe)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		parent = filepath.Join(parent, missing[index])
	}
	return parent, nil
}

// canonicalExisting walks directory entries rather than trusting EvalSymlinks:
// this both rejects links and makes a case-alias observable on every OS.
func canonicalExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	vol := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, vol)
	current := vol + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(rest, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		entry, exact := findEntry(entries, part)
		if entry == nil {
			return "", os.ErrNotExist
		}
		if !exact {
			return "", ErrCaseAlias
		}
		candidate := filepath.Join(current, entry.Name())
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", ErrSymlink
		}
		if info.Mode().IsRegular() && statNlink(info) > 1 {
			return "", ErrHardlink
		}
		current = candidate
	}
	return filepath.Clean(current), nil
}

func statNlink(info os.FileInfo) uint64 {
	return platformNlink(info)
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

func identityOfInfo(info os.FileInfo) (fileIdentity, error) {
	return platformIdentity(info)
}

func identityOfPath(path string) (fileIdentity, error) {
	probe := path
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			return identityOfInfo(info)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fileIdentity{}, err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fileIdentity{}, err
		}
		probe = parent
	}
}

func sameDevice(path string, rootID fileIdentity) error {
	if rootID.device == 0 || rootID.inode == 0 {
		return ErrUnsupportedIdentity
	}
	identity, err := identityOfPath(path)
	if err != nil {
		return err
	}
	if identity.device != rootID.device {
		return ErrDifferentDevice
	}
	return nil
}

func findEntry(entries []os.DirEntry, wanted string) (os.DirEntry, bool) {
	for _, entry := range entries {
		if entry.Name() == wanted {
			return entry, true
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), wanted) {
			return entry, false
		}
	}
	return nil, false
}

func isDescendant(child, root string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
