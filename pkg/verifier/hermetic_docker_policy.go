package verifier

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	hermeticGoVersion           = "1.25.0"
	hermeticGoToolchain         = "local"
	hermeticXSysVersion         = "v0.46.0"
	hermeticXSysZipHash         = "h1:noSf2Fq6F8DBgS+LysIkx7rIExoNHJsxOAtPp4rthXw="
	hermeticXSysModHash         = "h1:4GL1E5IUh+htKOUEOaiffhrAeqysfVGipDYzABqnCmw="
	hermeticNativeSourceRelPath = "pkg/verifier/fac151_native_integration_test.go"
)

// hermeticImagePin is the fixed golang image for one linux architecture.
// Production resolves the host GOARCH so CI (amd64) and local Colima (arm64)
// both have a reachable hermetic executor.
type hermeticImagePin struct {
	Platform     string // docker --platform value, e.g. linux/arm64
	Architecture string // docker inspect Architecture, e.g. arm64
	Image        string // golang@sha256:... (single-platform manifest digest)
	ConfigDigest string // image config/id digest accepted by InspectImage
	MediaType    string // registry mediaType of Image (manifest, not index)
}

// Known registry media types for Docker/OCI manifests and indexes.
const (
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIImageIndex      = "application/vnd.oi.image.index.v1+json"
)

// isImageManifestMediaType returns true for single-platform manifest types
// and false for multi-arch index types or anything else.
func isImageManifestMediaType(mt string) bool {
	switch mt {
	case mediaTypeDockerManifest, mediaTypeOCIManifest:
		return true
	default:
		return false
	}
}

// isImageIndexMediaType returns true for multi-arch index types.
func isImageIndexMediaType(mt string) bool {
	switch mt {
	case mediaTypeDockerManifestList, mediaTypeOCIImageIndex:
		return true
	default:
		return false
	}
}

// Pinned official library/golang:1.25.0 images (GOTOOLCHAIN=local). Digests
// were recorded from docker image inspect --platform linux/<arch>.
var hermeticImagePins = map[string]hermeticImagePin{
	"arm64": {
		Platform:     "linux/arm64",
		Architecture: "arm64",
		Image:        "golang@sha256:10e3849906212f513e105ab93ade96acd02dc30172adaf346c72ff82b003944b",
		ConfigDigest: "sha256:0d8d3155f6d8ef0ddc858877343d69c3683ac0807e218a7b28bb24e31ae9973d",
		MediaType:    mediaTypeOCIManifest,
	},
	"amd64": {
		Platform:     "linux/amd64",
		Architecture: "amd64",
		// The previous Image digest (5502b0e5...) was the multi-arch INDEX, not a
		// platform image: `docker manifest inspect` shows it carrying 16 manifests
		// and no config. Docker on Linux then reports Id as the resolved amd64
		// CONFIG digest, which matched neither the index nor the value pinned as
		// ConfigDigest -- and that stale value (f7414a0d...) was in fact the amd64
		// MANIFEST digest, one level off. CI failed as "manifest/config evidence
		// violates fixed policy" while a Darwin daemon, which reports Id as the
		// pulled reference, passed.
		//
		// Now symmetric with the arm64 pin: Image is the single-platform manifest
		// digest, ConfigDigest is that manifest's config digest. Both verified via
		// docker manifest inspect and a --platform linux/amd64 pull.
		Image:        "golang@sha256:f7414a0dc5a64713686cbc9f1e8a7379b66af63ef9ad15760b43db40e0b15d9c",
		ConfigDigest: "sha256:749aa15fc6e4fac30c91f785daf525be2b791065e7eb225a43de6492ed2d03c0",
		MediaType:    mediaTypeOCIManifest,
	},
}

// Package-level aliases resolve from the host architecture so existing tests
// that reference hermeticDockerImage/Platform track the local pin. Production
// code paths use hermeticDockerPolicy.pin explicitly.
var (
	hermeticDockerPlatform     string
	hermeticDockerImage        string
	hermeticDockerConfigDigest string
)

func init() {
	pin, err := hermeticImagePinFor(runtime.GOARCH)
	if err != nil {
		return
	}
	hermeticDockerPlatform = pin.Platform
	hermeticDockerImage = pin.Image
	hermeticDockerConfigDigest = pin.ConfigDigest
}

func hermeticImagePinFor(arch string) (hermeticImagePin, error) {
	switch arch {
	case "arm64", "amd64":
		pin, ok := hermeticImagePins[arch]
		if !ok || pin.Image == "" || pin.Platform == "" || pin.Architecture == "" || pin.ConfigDigest == "" || pin.MediaType == "" {
			return hermeticImagePin{}, fmt.Errorf("FAC-151 hermetic image pin for %s is incomplete", arch)
		}
		return pin, nil
	default:
		return hermeticImagePin{}, fmt.Errorf("FAC-151 hermetic profile has no image pin for GOARCH=%s (supported: arm64, amd64)", arch)
	}
}

var hermeticFAC151Allowlist = [...]string{
	"TestExecuteCancellationKillsProcessGroup",
	"TestExecuteCancellationWithoutReadyBarrierCannotProveDescendant",
	"TestExecuteCancellationRequiresProcessGroupReap",
	"TestExecuteCancellationReadyBarrierSelectorRuns",
	"TestExecuteSuccessWithBackgroundWriterBlocksAndReaps",
	"TestExecuteMutationOmittingFinalizeOwnedTreeReturnsTooEarly",
	"TestExecuteCancelAfterStartClosesProcessGroup",
	"TestOwnershipMarkerLockTracksLastInheritedHolder",
	"TestMarkerLineageFindsSetsidChdirAwayWriter",
	"TestUnrelatedPathContactWithoutMarkerIsNotLineage",
	"TestExecuteDetachedOnlySessionWriterBlocksAndReaps",
	"TestExecuteUnrelatedPathContactSurvivesMarkedWriterReaped",
	"TestExecuteDetachedOnlyMutationRemovingMarkerDrainLeavesWriter",
	"TestExecuteDetachedSessionAndBackgroundWriters",
	"TestProcTokenIdentityBoundRefusesStalePID",
	"TestKillProcessGroupMembersNeverUsesNegativePGID",
	"TestOwnedNeverReplacesTokenOnPIDReuse",
	"TestOwnedFreezeRejectsPostLeaderGroupAdoption",
	"TestIsExpectedKillWaitUsesTypedWaitStatus",
	"TestLateWriterIntoGitRequiresExplicitReap",
	"TestLateWriterCleanupMutationOmittingReapStillFails",
	"TestProcessGroupReapAllowsTempDirCleanup",
	"TestReapOwnedCmdKillsGrandchildren",
	"TestReapOwnedCmdTreeCloseKillsGrandchildrenDespiteLeaderOnlyGroupKill",
	"TestFinalizeOwnedTreeMutationLeavesGrandchildAlive",
}

type hermeticDockerPolicy struct {
	pin hermeticImagePin
}

// fixedHermeticDockerPolicy resolves the pin for the host GOARCH. Callers on
// unsupported architectures fail closed when the pin is empty.
func fixedHermeticDockerPolicy() hermeticDockerPolicy {
	pin, err := hermeticImagePinFor(runtime.GOARCH)
	if err != nil {
		return hermeticDockerPolicy{}
	}
	return hermeticDockerPolicy{pin: pin}
}

func hermeticDockerPolicyForArch(arch string) (hermeticDockerPolicy, error) {
	pin, err := hermeticImagePinFor(arch)
	if err != nil {
		return hermeticDockerPolicy{}, err
	}
	return hermeticDockerPolicy{pin: pin}, nil
}

func (p hermeticDockerPolicy) image() string        { return p.pin.Image }
func (p hermeticDockerPolicy) platform() string     { return p.pin.Platform }
func (p hermeticDockerPolicy) configDigest() string { return p.pin.ConfigDigest }
func (p hermeticDockerPolicy) architecture() string { return p.pin.Architecture }
func (p hermeticDockerPolicy) valid() bool {
	return p.pin.Image != "" && p.pin.Platform != "" && p.pin.Architecture != "" && p.pin.ConfigDigest != "" && p.pin.MediaType != ""
}

func verifyFAC151Allowlist(sourceRoot string) error {
	path := filepath.Join(sourceRoot, hermeticNativeSourceRelPath)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return fmt.Errorf("parse FAC-151 native source: %w", err)
	}
	declared := make([]string, 0, len(hermeticFAC151Allowlist))
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
			declared = append(declared, function.Name.Name)
		}
	}
	sort.Strings(declared)
	expected := append([]string(nil), hermeticFAC151Allowlist[:]...)
	sort.Strings(expected)
	if len(declared) != len(expected) {
		return fmt.Errorf("FAC-151 allowlist drift: declared=%d expected=%d", len(declared), len(expected))
	}
	for index := range expected {
		if declared[index] != expected[index] {
			return fmt.Errorf("FAC-151 allowlist drift at %d: declared=%q expected=%q", index, declared[index], expected[index])
		}
	}
	return nil
}

type verifiedGoCache struct {
	Zip     []byte
	ZipHash []byte
	Mod     []byte
	Info    []byte
}

func verifiedHostGoCache() (verifiedGoCache, error) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return verifiedGoCache{}, fmt.Errorf("resolve account home for Go cache: %w", err)
	}
	home := account.HomeDir
	if home == "" || !filepath.IsAbs(home) {
		return verifiedGoCache{}, errors.New("account database returned an invalid home directory")
	}
	root := filepath.Join(home, "go", "pkg", "mod", "cache", "download", "golang.org", "x", "sys", "@v")
	zipBytes, err := readImmutableBounded(filepath.Join(root, hermeticXSysVersion+".zip"), 4<<20)
	if err != nil {
		return verifiedGoCache{}, err
	}
	zipHashBytes, err := readImmutableBounded(filepath.Join(root, hermeticXSysVersion+".ziphash"), 256)
	if err != nil {
		return verifiedGoCache{}, err
	}
	modBytes, err := readImmutableBounded(filepath.Join(root, hermeticXSysVersion+".mod"), 64<<10)
	if err != nil {
		return verifiedGoCache{}, err
	}
	infoBytes, err := readImmutableBounded(filepath.Join(root, hermeticXSysVersion+".info"), 4096)
	if err != nil {
		return verifiedGoCache{}, err
	}
	zipHash, err := goZipHash(zipBytes)
	if err != nil || zipHash != hermeticXSysZipHash {
		return verifiedGoCache{}, fmt.Errorf("Go cache zip hash mismatch: got %q want %q err=%v", zipHash, hermeticXSysZipHash, err)
	}
	if strings.TrimSpace(string(zipHashBytes)) != hermeticXSysZipHash {
		return verifiedGoCache{}, fmt.Errorf("Go cache ziphash mismatch: got %q want %q err=%v", strings.TrimSpace(string(zipHashBytes)), hermeticXSysZipHash, err)
	}
	if got := goModHash(modBytes); got != hermeticXSysModHash {
		return verifiedGoCache{}, fmt.Errorf("Go cache mod hash mismatch: got %q want %q", got, hermeticXSysModHash)
	}
	var info struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(infoBytes, &info); err != nil || info.Version != hermeticXSysVersion {
		return verifiedGoCache{}, fmt.Errorf("Go cache info version mismatch: got %q want %q err=%v", info.Version, hermeticXSysVersion, err)
	}
	return verifiedGoCache{Zip: zipBytes, ZipHash: zipHashBytes, Mod: modBytes, Info: infoBytes}, nil
}

func goContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:])
}

func goModHash(content []byte) string {
	fileSum := sha256.Sum256(content)
	line := hex.EncodeToString(fileSum[:]) + "  go.mod\n"
	return goContentHash([]byte(line))
}

func goZipHash(content []byte) (string, error) {
	const maxZipFiles = 4096
	const maxZipFileBytes = 16 << 20
	const maxZipTotalBytes = 16 << 20
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", err
	}
	records := make(map[string]string, len(archive.File))
	seen := make(map[string]struct{}, len(archive.File))
	var totalBytes uint64
	if len(archive.File) > maxZipFiles {
		return "", errors.New("Go cache zip contains too many files")
	}
	for _, file := range archive.File {
		cleanName := path.Clean(file.Name)
		if file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() ||
			file.Name == "" || strings.ContainsAny(file.Name, "\x00\n\\") || filepath.IsAbs(file.Name) || filepath.VolumeName(file.Name) != "" || (len(file.Name) > 1 && file.Name[1] == ':') ||
			cleanName != file.Name || cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
			return "", errors.New("Go cache zip contains an unsafe entry")
		}
		if _, exists := seen[file.Name]; exists {
			return "", errors.New("Go cache zip contains duplicate names")
		}
		seen[file.Name] = struct{}{}
		if file.UncompressedSize64 > maxZipFileBytes || totalBytes+file.UncompressedSize64 > maxZipTotalBytes {
			return "", errors.New("Go cache zip exceeds compiled size bounds")
		}
		reader, err := file.Open()
		if err != nil {
			return "", err
		}
		hash := sha256.New()
		count, readErr := io.Copy(hash, io.LimitReader(reader, int64(maxZipFileBytes)+1))
		closeErr := reader.Close()
		if readErr != nil {
			return "", readErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if count < 0 || count > int64(maxZipFileBytes) || uint64(count) != file.UncompressedSize64 {
			return "", errors.New("Go cache zip entry size mismatch")
		}
		totalBytes += uint64(count)
		records[file.Name] = hex.EncodeToString(hash.Sum(nil))
	}
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines strings.Builder
	for _, name := range names {
		fmt.Fprintf(&lines, "%s  %s\n", records[name], name)
	}
	return goContentHash([]byte(lines.String())), nil
}

func readImmutableBounded(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open immutable cache file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("cache input is not a regular file")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, errors.New("bounded cache file exceeds limit")
	}
	return data, nil
}

func verifiedGoCacheTar(cache verifiedGoCache) ([]byte, error) {
	files := []struct {
		name string
		data []byte
	}{
		{hermeticXSysVersion + ".zip", cache.Zip},
		{hermeticXSysVersion + ".ziphash", cache.ZipHash},
		{hermeticXSysVersion + ".mod", cache.Mod},
		{hermeticXSysVersion + ".info", cache.Info},
	}
	payload := make([]tarPayload, 0, len(files))
	for _, file := range files {
		payload = append(payload, tarPayload{name: file.name, data: file.data, mode: 0o444})
	}
	return boundedTarPayload(payload, maxHermeticCacheTarBytes)
}
