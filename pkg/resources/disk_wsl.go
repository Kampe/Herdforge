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
)

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
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/WSL"); err == nil {
		return true
	}
	if _, err := os.Stat("/usr/lib/wsl"); err == nil {
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

// parseLxssRegistryOutput parses reg.exe query output for the Lxss registry tree.
// It tracks distributions by GUID and DistributionName. If an unsupported value encoding is
// encountered for BasePath or DistributionName, it records an explicit diagnostic.
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

			switch strings.ToLower(name) {
			case "defaultdistribution":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					defaultGUID = strings.TrimSpace(val)
				}
			case "distributionname":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					currentDistro.DistributionName = strings.TrimSpace(val)
				} else {
					currentDistro.EncodingError = fmt.Sprintf("unsupported registry type %s for DistributionName", valType)
				}
			case "basepath":
				if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
					cleanVal := strings.TrimSpace(val)
					currentDistro.BasePath = cleanVal
					currentDistro.DriveLetter = extractDriveLetter(cleanVal)
				} else {
					currentDistro.EncodingError = fmt.Sprintf("unsupported registry type %s for BasePath", valType)
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
// It verifies that the mount is an authentic DrvFS or 9p mount for the requested drive letter,
// rejecting ext4/tmpfs mounts or spoofed paths.
func findDriveMountPath(driveLetter string, mountsData []byte) (string, error) {
	drivePrefix := strings.ToUpper(strings.TrimSuffix(driveLetter, ":"))
	if len(drivePrefix) != 1 || drivePrefix[0] < 'A' || drivePrefix[0] > 'Z' {
		return "", fmt.Errorf("invalid drive letter %q", driveLetter)
	}

	lines := strings.Split(string(mountsData), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mountPoint := fields[1]
		fsType := fields[2]
		opts := ""
		if len(fields) >= 4 {
			opts = fields[3]
		}

		isDrvFS := fsType == "drvfs" || fsType == "9p" || strings.Contains(opts, "aname=drvfs")
		if !isDrvFS {
			continue
		}

		// Device field in /proc/mounts (e.g. "C:\134", "C:\", "C:", "c:")
		devClean := strings.ToUpper(device)
		devClean = strings.ReplaceAll(devClean, `\134`, `\`)
		devClean = strings.TrimSuffix(devClean, `\`)
		devClean = strings.TrimSuffix(devClean, `:`)

		if devClean == drivePrefix {
			return mountPoint, nil
		}

		// Mount options path check (e.g. "aname=drvfs;path=C:\;...")
		optsUpper := strings.ToUpper(opts)
		if strings.Contains(optsUpper, "PATH="+drivePrefix+`:\`) ||
			strings.Contains(optsUpper, "PATH="+drivePrefix+`:`) ||
			strings.Contains(optsUpper, "PATH="+drivePrefix+`;`) {
			return mountPoint, nil
		}

		// Canonical DrvFS mount point /mnt/<drive>
		if strings.HasPrefix(mountPoint, "/mnt/") && len(mountPoint) == 6 && strings.ToUpper(string(mountPoint[5])) == drivePrefix {
			return mountPoint, nil
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
		mountPoint := fields[1]
		fsType := fields[2]
		opts := ""
		if len(fields) >= 4 {
			opts = fields[3]
		}
		if fsType == "9p" || fsType == "drvfs" || strings.Contains(opts, "aname=drvfs") {
			if abs == mountPoint || strings.HasPrefix(abs, strings.TrimSuffix(mountPoint, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// probeHostVolumeCapacity runs an external bounded statfs observation against mountPath.
// To prevent indefinite hangs on degraded 9p/DrvFS mounts, it uses a context-bounded subprocess
// ("stat -f -c ...") which is killed if ctx expires.
func probeHostVolumeCapacity(ctx context.Context, mountPath string) (Capacity, error) {
	// First attempt bounded coreutils stat command which cleanly terminates on timeout.
	statBin, err := exec.LookPath("stat")
	if err == nil {
		cmd := exec.CommandContext(ctx, statBin, "-f", "-c", "%S %b %a %c %d", mountPath)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			fields := strings.Fields(strings.TrimSpace(stdout.String()))
			if len(fields) >= 5 {
				blockSize, e1 := strconv.ParseUint(fields[0], 10, 64)
				totalBlocks, e2 := strconv.ParseUint(fields[1], 10, 64)
				freeBlocks, e3 := strconv.ParseUint(fields[2], 10, 64)
				totalInodes, e4 := strconv.ParseUint(fields[3], 10, 64)
				freeInodes, e5 := strconv.ParseUint(fields[4], 10, 64)
				if e1 == nil && e2 == nil && e3 == nil && e4 == nil && e5 == nil && blockSize > 0 {
					return Capacity{
						FilesystemID: mountPath,
						TotalBytes:   blockSize * totalBlocks,
						FreeBytes:    blockSize * freeBlocks,
						TotalInodes:  totalInodes,
						FreeInodes:   freeInodes,
					}, nil
				}
			}
		} else if ctx.Err() != nil {
			return Capacity{}, fmt.Errorf("host volume probe timed out: %w", ctx.Err())
		}
	}

	// Fallback to in-process statfs if stat command is unavailable and context is not cancelled.
	if err := ctx.Err(); err != nil {
		return Capacity{}, fmt.Errorf("host volume probe context: %w", err)
	}
	return statFSUnix(mountPath)
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
