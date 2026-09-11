package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// wslProbeTimeout bounds all external Windows and 9p/drvfs probes to prevent hanging.
const wslProbeTimeout = 3 * time.Second

// Seams for hermetic testing and dependency injection.
var (
	wslDetectionOverride *bool
	wslRuntimeGOOS       = runtime.GOOS
	wslProcVersionReader = func() ([]byte, error) {
		return os.ReadFile("/proc/version")
	}
	wslProcMountsReader = func() ([]byte, error) {
		return os.ReadFile("/proc/mounts")
	}
	wslRegistryQueryExecutor = func(ctx context.Context) ([]byte, error) {
		regPath := "reg.exe"
		if p, err := exec.LookPath("reg.exe"); err == nil {
			regPath = p
		} else if _, err := os.Stat("/mnt/c/Windows/System32/reg.exe"); err == nil {
			regPath = "/mnt/c/Windows/System32/reg.exe"
		}
		cmd := exec.CommandContext(ctx, regPath, "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Lxss`, "/s")
		return cmd.Output()
	}
	wslDriveStatFS = func(ctx context.Context, mountPath string) (Capacity, error) {
		return probeHostVolumeCapacity(ctx, mountPath)
	}
	// wslSignalStat probes the filesystem markers isWSLEnvironment uses as
	// secondary signals. Seam: on a real WSL host these paths exist, so tests
	// asserting the "no signals" branch must inject their own answer or they
	// assert a property of the machine they run on (FAC-215, FAC-613).
	wslSignalStat = func(path string) error {
		_, err := os.Stat(path)
		return err
	}
)

// osBackendStatFSOverride, when non-nil, replaces OSBackend.StatFS entirely.
// It is set only through SetOSBackendStatFSForTest; nil in production.
var osBackendStatFSOverride func(path string) (Capacity, error)

// guestStatFSProbe is the guest-side capacity probe OSBackend.StatFS runs
// before applying the WSL host-volume bound. It defaults to the platform
// statFSUnix syscall and is replaced only by tests in this package through
// withGuestStatFSProbe, whose restore func must be deferred. Production call
// sites are unchanged and no public API is added (FAC-810).
var guestStatFSProbe = statFSUnix

// withGuestStatFSProbe swaps the guest capacity probe for the duration of a
// test and returns the restore func. Unlike SetOSBackendStatFSForTest it
// does not replace OSBackend.StatFS: the real StatFS-to-boundWSLCapacity
// wiring stays under test with synthetic guest and host capacities.
func withGuestStatFSProbe(fn func(path string) (Capacity, error)) (restore func()) {
	prev := guestStatFSProbe
	guestStatFSProbe = fn
	return func() { guestStatFSProbe = prev }
}

// SetOSBackendStatFSForTest pins the filesystem capacity OSBackend reports,
// for out-of-package tests whose subjects construct the default capacity gate
// (worktree, harvest, verifier, cmd/herd integration suites). Those tests
// create real worktrees under temp dirs and must not assert a property of the
// machine they run on (FAC-215): at a8cd39e15c85 `make ci` failed ~90 of them
// on a real WSL host whose probe fails closed and whose physical drive sits
// below the 15 GiB reserve, and the same suites fail on any macOS host under
// the 2%% reserve. Gate BEHAVIOR is covered by each package's injected-fake
// gate tests; production is unchanged — the host-volume bound and the reserve
// policy still apply wherever this hook is not installed. Returns a restore
// func.
func SetOSBackendStatFSForTest(fn func(path string) (Capacity, error)) func() {
	prev := osBackendStatFSOverride
	osBackendStatFSOverride = fn
	return func() { osBackendStatFSOverride = prev }
}

// HermeticStatFSForTest is the canned healthy filesystem for
// SetOSBackendStatFSForTest: fixed, plentiful, and identical on every host.
func HermeticStatFSForTest(path string) (Capacity, error) {
	return Capacity{
		FilesystemID: "hermetic-test-fs",
		TotalBytes:   512 << 30,
		FreeBytes:    256 << 30,
		TotalInodes:  1 << 24,
		FreeInodes:   1 << 23,
	}, nil
}

type wslDistroInfo struct {
	GUID             string
	DistributionName string
	BasePath         string
	DriveLetter      string
	EncodingError    string
}

// isWSLEnvironment detects if the current process is running inside Windows Subsystem for Linux.
// It checks /proc/version first, and if atypical or unreadable, inspects secondary strong WSL signals.
func isWSLEnvironment() bool {
	if wslDetectionOverride != nil {
		return *wslDetectionOverride
	}
	if wslRuntimeGOOS != "linux" {
		return false
	}
	data, err := wslProcVersionReader()
	if err == nil {
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			return true
		}
	}
	// Atypical kernel or missing /proc/version: check strong secondary WSL signals.
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	if wslSignalStat("/proc/sys/fs/binfmt_misc/WSLInterop") == nil {
		return true
	}
	if wslSignalStat("/run/WSL") == nil {
		return true
	}
	if wslSignalStat("/usr/lib/wsl") == nil {
		return true
	}
	return false
}

// extractDriveLetter extracts a Windows drive letter (e.g. "C:", "D:") from a Windows path.
// It strips extended-path prefixes like "\\?\" and validates the single ASCII drive letter.
func extractDriveLetter(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, `\\?\`)
	if len(path) >= 2 && path[1] == ':' {
		ch := path[0]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
			return strings.ToUpper(string(ch)) + ":"
		}
	}
	return ""
}

// decodeProcfsEscape decodes standard 3-digit octal escape sequences (e.g. \040 -> space, \134 -> \)
// used by Linux /proc/mounts.
func decodeProcfsEscape(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			o1, o2, o3 := s[i+1], s[i+2], s[i+3]
			if o1 >= '0' && o1 <= '7' && o2 >= '0' && o2 <= '7' && o3 >= '0' && o3 <= '7' {
				val := (o1-'0')*64 + (o2-'0')*8 + (o3 - '0')
				buf.WriteByte(byte(val))
				i += 4
				continue
			}
		}
		buf.WriteByte(s[i])
		i++
	}
	return buf.String()
}

// parseDriveLetterFromDevice extracts drive letter if device explicitly names a Windows drive (e.g. "D:\134", "D:\", "D:").
func parseDriveLetterFromDevice(device string) string {
	clean := decodeProcfsEscape(device)
	clean = strings.TrimSpace(clean)
	return extractDriveLetter(clean)
}

// parseDriveLetterFromOptions extracts drive letter from drvfs options tokens (e.g. path=D:\ or path=C:).
func parseDriveLetterFromOptions(opts string) (string, bool) {
	decoded := decodeProcfsEscape(opts)
	tokens := strings.FieldsFunc(decoded, func(r rune) bool {
		return r == ',' || r == ';'
	})
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if strings.HasPrefix(strings.ToLower(tok), "path=") {
			val := strings.TrimPrefix(tok, tok[:5])
			dl := extractDriveLetter(val)
			if dl != "" {
				return dl, true
			}
		}
	}
	return "", false
}

// checkedMul performs safe uint64 multiplication and reports overflow.
func checkedMul(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	c := a * b
	if c/a != b {
		return 0, true
	}
	return c, false
}

// parseLxssRegistryOutput parses reg.exe query output for the Lxss registry tree.
// It validates registry value types (REG_SZ, REG_EXPAND_SZ) and byte encodings (valid UTF-8, no embedded nulls).
// Any unsupported value type or invalid byte encoding is explicitly flagged.
func parseLxssRegistryOutput(output string) (map[string]wslDistroInfo, string, error) {
	distros := make(map[string]wslDistroInfo)
	var defaultGUID string

	lines := strings.Split(output, "\n")
	var currentGUID string
	var currentDistro wslDistroInfo

	saveCurrent := func() {
		if currentDistro.GUID != "" || currentDistro.DistributionName != "" || currentDistro.BasePath != "" {
			if currentDistro.DistributionName != "" {
				distros[currentDistro.DistributionName] = currentDistro
			}
			if currentDistro.GUID != "" {
				distros[currentDistro.GUID] = currentDistro
			}
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Lxss") {
			saveCurrent()
			currentDistro = wslDistroInfo{}
			parts := strings.Split(line, "\\")
			if len(parts) > 0 {
				last := parts[len(parts)-1]
				if strings.HasPrefix(last, "{") && strings.HasSuffix(last, "}") {
					currentGUID = last
					currentDistro.GUID = currentGUID
				} else {
					currentGUID = ""
				}
			}
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			name := fields[0]
			valType := fields[1]
			val := ""
			if len(fields) >= 3 {
				val = strings.Join(fields[2:], " ")
			}

			// Validate byte encoding
			if !utf8.ValidString(val) || strings.ContainsRune(val, 0) {
				currentDistro.EncodingError = fmt.Sprintf("invalid byte encoding in registry value %s", name)
				continue
			}

			switch strings.ToLower(name) {
			case "defaultdistribution":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					defaultGUID = strings.TrimSpace(val)
				}
			case "distributionname":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					currentDistro.DistributionName = strings.TrimSpace(val)
				} else {
					currentDistro.EncodingError = fmt.Sprintf("unsupported registry value type %s for DistributionName", valType)
				}
			case "basepath":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					cleanVal := strings.TrimSpace(val)
					currentDistro.BasePath = cleanVal
					currentDistro.DriveLetter = extractDriveLetter(cleanVal)
				} else {
					currentDistro.EncodingError = fmt.Sprintf("unsupported registry value type %s for BasePath", valType)
				}
			}
		}
	}
	saveCurrent()
	return distros, defaultGUID, nil
}

// getUniqueDistros returns the set of unique distributions (deduplicated by GUID/BasePath).
func getUniqueDistros(distros map[string]wslDistroInfo) []wslDistroInfo {
	seen := make(map[string]bool)
	var unique []wslDistroInfo
	for _, info := range distros {
		key := info.GUID
		if key == "" {
			key = info.BasePath
		}
		if key == "" {
			key = info.DistributionName
		}
		if key != "" && !seen[key] {
			seen[key] = true
			unique = append(unique, info)
		}
	}
	return unique
}

// resolveDistroBackingDrive determines the physical Windows drive letter backing the WSL distro.
// It enforces fail-closed semantics:
// 1. If WSL_DISTRO_NAME is explicitly set, it MUST match a registered distro with a valid drive letter.
//    If missing or invalid, it returns an error and NEVER falls back to default or another distro.
// 2. If WSL_DISTRO_NAME is empty, it succeeds ONLY if exactly one unique distro is registered.
//    If multiple distros exist, it fails closed to prevent non-deterministic drive selection.
func resolveDistroBackingDrive(ctx context.Context) (string, error) {
	distroName := os.Getenv("WSL_DISTRO_NAME")
	out, err := wslRegistryQueryExecutor(ctx)
	if err != nil {
		return "", fmt.Errorf("query lxss registry: %w", err)
	}

	distros, _, err := parseLxssRegistryOutput(string(out))
	if err != nil {
		return "", err
	}
	if len(distros) == 0 {
		return "", errors.New("no wsl distributions found in lxss registry")
	}

	// 1. Explicit WSL_DISTRO_NAME provided: MUST match in registry.
	if distroName != "" {
		info, ok := distros[distroName]
		if !ok {
			return "", fmt.Errorf("wsl distro %q not found in Lxss registry", distroName)
		}
		if info.EncodingError != "" {
			return "", fmt.Errorf("wsl distro %q registry encoding error: %s", distroName, info.EncodingError)
		}
		if info.DriveLetter == "" {
			return "", fmt.Errorf("wsl distro %q has invalid or unparseable BasePath %q", distroName, info.BasePath)
		}
		return info.DriveLetter, nil
	}

	// 2. WSL_DISTRO_NAME is absent: require exactly one unambiguous unique distro.
	unique := getUniqueDistros(distros)
	if len(unique) == 1 {
		info := unique[0]
		if info.EncodingError != "" {
			return "", fmt.Errorf("wsl distro %q registry encoding error: %s", info.DistributionName, info.EncodingError)
		}
		if info.DriveLetter == "" {
			return "", fmt.Errorf("wsl distro %q has invalid or unparseable BasePath %q", info.DistributionName, info.BasePath)
		}
		return info.DriveLetter, nil
	}

	// Multiple distros exist and running distro identity is unauthenticated: fail closed.
	return "", fmt.Errorf("wsl distro name is empty and %d distributions are registered; cannot determine backing host drive", len(unique))
}

// findDriveMountPath locates the Linux mount point corresponding to a Windows drive letter.
// It verifies that the mount is an authentic DrvFS or 9p mount for the requested drive letter:
// - Matches device and/or options token boundaries (e.g. path=D:\),
// - Rejects contradictory device vs options,
// - Eliminates mountpoint-only fallbacks,
// - Decodes procfs octal escape sequences for mountpoints with spaces.
func findDriveMountPath(driveLetter string, mountsData []byte) (string, error) {
	drivePrefix := strings.ToUpper(strings.TrimSuffix(driveLetter, ":"))
	if len(drivePrefix) != 1 || drivePrefix[0] < 'A' || drivePrefix[0] > 'Z' {
		return "", fmt.Errorf("invalid drive letter %q", driveLetter)
	}
	targetDrive := drivePrefix + ":"

	lines := strings.Split(string(mountsData), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		rawDevice := fields[0]
		rawMountPoint := fields[1]
		fsType := fields[2]
		rawOpts := ""
		if len(fields) >= 4 {
			rawOpts = fields[3]
		}

		isDrvFS := fsType == "drvfs" || fsType == "9p" || strings.Contains(rawOpts, "aname=drvfs")
		if !isDrvFS {
			continue
		}

		devDrive := parseDriveLetterFromDevice(rawDevice)
		optsDrive, hasOptsDrive := parseDriveLetterFromOptions(rawOpts)

		// Reject contradictory device vs options (e.g. device is D: but options specify path=D:\)
		if devDrive != "" && hasOptsDrive && devDrive != optsDrive {
			continue
		}

		matched := false
		if devDrive == targetDrive {
			matched = true
		} else if hasOptsDrive && optsDrive == targetDrive {
			matched = true
		}

		if matched {
			return decodeProcfsEscape(rawMountPoint), nil
		}
	}

	return "", fmt.Errorf("mount point for Windows drive %s not found in /proc/mounts", driveLetter)
}

// isPathOnDrvFS checks if a target path is already located on a Windows drvfs/9p mount.
func isPathOnDrvFS(path string, mountsData []byte) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	lines := strings.Split(string(mountsData), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		rawMountPoint := fields[1]
		fsType := fields[2]
		rawOpts := ""
		if len(fields) >= 4 {
			rawOpts = fields[3]
		}
		if fsType == "9p" || fsType == "drvfs" || strings.Contains(rawOpts, "aname=drvfs") {
			mountPoint := decodeProcfsEscape(rawMountPoint)
			if abs == mountPoint || strings.HasPrefix(abs, strings.TrimSuffix(mountPoint, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// probeHostVolumeCapacity runs an external bounded statfs observation against mountPath.
// It uses a context-bounded subprocess ("stat -f -c ...") which cleanly terminates if ctx expires.
// Unbounded in-process statfs is never executed on 9p/drvfs to prevent kernel D-state hangs.
func probeHostVolumeCapacity(ctx context.Context, mountPath string) (Capacity, error) {
	statBin, err := exec.LookPath("stat")
	if err != nil {
		return Capacity{}, fmt.Errorf("host volume probe tool unavailable: %w", err)
	}

	cmd := exec.CommandContext(ctx, statBin, "-f", "-c", "%S %b %a %c %d", mountPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Bound the CALLER, not just the child: Run waits for stdout/stderr pipe
	// I/O, so a stat wedged in an uninterruptible 9p D-state wait (SIGKILL
	// pending but undeliverable) — or any descendant inheriting the pipes —
	// would block past ctx despite the kill. WaitDelay abandons the pipes
	// shortly after ctx expiry so the probe returns on the deadline.
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Capacity{}, fmt.Errorf("host volume probe timed out: %w", ctx.Err())
		}
		return Capacity{}, fmt.Errorf("host volume probe command failed (%s): %w", strings.TrimSpace(stderr.String()), err)
	}

	fields := strings.Fields(strings.TrimSpace(stdout.String()))
	if len(fields) < 5 {
		return Capacity{}, fmt.Errorf("host volume probe returned unexpected output %q", stdout.String())
	}

	blockSize, e1 := strconv.ParseUint(fields[0], 10, 64)
	totalBlocks, e2 := strconv.ParseUint(fields[1], 10, 64)
	freeBlocks, e3 := strconv.ParseUint(fields[2], 10, 64)
	totalInodes, e4 := strconv.ParseUint(fields[3], 10, 64)
	freeInodes, e5 := strconv.ParseUint(fields[4], 10, 64)

	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		return Capacity{}, fmt.Errorf("host volume probe returned invalid integer fields: %v", fields)
	}

	if blockSize == 0 || totalBlocks == 0 {
		return Capacity{}, fmt.Errorf("host volume probe reported zero block size or total blocks (blockSize=%d, totalBlocks=%d)", blockSize, totalBlocks)
	}

	if freeBlocks > totalBlocks {
		return Capacity{}, fmt.Errorf("host volume probe reported free blocks %d > total blocks %d", freeBlocks, totalBlocks)
	}

	totalBytes, overflow1 := checkedMul(blockSize, totalBlocks)
	if overflow1 {
		return Capacity{}, fmt.Errorf("host volume probe total bytes overflow (%d * %d)", blockSize, totalBlocks)
	}

	freeBytes, overflow2 := checkedMul(blockSize, freeBlocks)
	if overflow2 {
		return Capacity{}, fmt.Errorf("host volume probe free bytes overflow (%d * %d)", blockSize, freeBlocks)
	}

	return Capacity{
		FilesystemID: mountPath,
		TotalBytes:   totalBytes,
		FreeBytes:    freeBytes,
		TotalInodes:  totalInodes,
		FreeInodes:   freeInodes,
	}, nil
}

// boundWSLCapacity bounds guest filesystem capacity by the actual physical Windows host volume.
//
// Total Semantics:
// - FreeBytes is capped to min(guestFreeBytes, hostFreeBytes). In WSL2, guest ext4 filesystems
//   dynamically grow on the host VHD, so guest free space cannot exceed available physical host bytes.
// - TotalBytes is capped to min(guestTotalBytes, hostTotalBytes) to reflect real physical limits.
// - If backing host capacity cannot be determined or probed, it fails closed with an explicit error.
func boundWSLCapacity(guestCap Capacity, path string) (Capacity, error) {
	if !isWSLEnvironment() {
		return guestCap, nil
	}

	mounts, err := wslProcMountsReader()
	if err == nil && isPathOnDrvFS(path, mounts) {
		return guestCap, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), wslProbeTimeout)
	defer cancel()

	hostDrive, err := resolveDistroBackingDrive(ctx)
	if err != nil {
		return Capacity{}, fmt.Errorf("wsl host backing volume detection: %w", err)
	}

	mountPath, err := findDriveMountPath(hostDrive, mounts)
	if err != nil {
		return Capacity{}, fmt.Errorf("wsl host backing volume mount for %s: %w", hostDrive, err)
	}

	hostCap, err := wslDriveStatFS(ctx, mountPath)
	if err != nil {
		return Capacity{}, fmt.Errorf("wsl host backing volume statfs %s (%s): %w", hostDrive, mountPath, err)
	}

	if hostCap.TotalBytes == 0 {
		return Capacity{}, fmt.Errorf("wsl host backing volume %s reported invalid zero capacity", hostDrive)
	}

	// Cap available guest capacity by physical host volume free bytes.
	if hostCap.FreeBytes < guestCap.FreeBytes {
		guestCap.FreeBytes = hostCap.FreeBytes
	}
	if hostCap.TotalBytes < guestCap.TotalBytes {
		guestCap.TotalBytes = hostCap.TotalBytes
	}

	return guestCap, nil
}
