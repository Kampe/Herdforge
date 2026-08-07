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
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	hermeticDockerPlatform      = "linux/arm64"
	hermeticDockerImage         = "golang@sha256:10e3849906212f513e105ab93ade96acd02dc30172adaf346c72ff82b003944b"
	hermeticDockerConfigDigest  = "sha256:0d8d3155f6d8ef0ddc858877343d69c3683ac0807e218a7b28bb24e31ae9973d"
	hermeticGoVersion           = "1.25.0"
	hermeticGoToolchain         = "local"
	hermeticXSysVersion         = "v0.46.0"
	hermeticXSysZipHash         = "h1:noSf2Fq6F8DBgS+LysIkx7rIExoNHJsxOAtPp4rthXw="
	hermeticXSysModHash         = "h1:4GL1E5IUh+htKOUEOaiffhrAeqysfVGipDYzABqnCmw="
	hermeticNativeSourceRelPath = "pkg/verifier/fac151_native_integration_test.go"
)

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

type hermeticDockerPolicy struct{}

func fixedHermeticDockerPolicy() hermeticDockerPolicy { return hermeticDockerPolicy{} }

func (hermeticDockerPolicy) image() string        { return hermeticDockerImage }
func (hermeticDockerPolicy) platform() string     { return hermeticDockerPlatform }
func (hermeticDockerPolicy) configDigest() string { return hermeticDockerConfigDigest }

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
		if count > int64(maxZipFileBytes) || uint64(count) != file.UncompressedSize64 {
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
