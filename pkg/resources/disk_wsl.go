package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// wslProbeTimeout bounds all external Windows queries to prevent hanging.
const wslProbeTimeout = 3 * time.Second

// Seams for hermetic testing and dependency injection.
var (
	wslDetectionOverride *bool
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
	wslDriveStatFS = func(mountPath string) (Capacity, error) {
		return statFSUnix(mountPath)
	}
)

type wslDistroInfo struct {
	GUID             string
	DistributionName string
	BasePath         string
	DriveLetter      string
}

// isWSLEnvironment detects if the current process is running inside Windows Subsystem for Linux.
func isWSLEnvironment() bool {
	if wslDetectionOverride != nil {
		return *wslDetectionOverride
	}
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := wslProcVersionReader()
	if err != nil {
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			return true
		}
		if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
			return true
		}
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// extractDriveLetter extracts a Windows drive letter (e.g. "C:", "D:") from a Windows path.
func extractDriveLetter(path string) string {
	path = strings.TrimSpace(path)
	if len(path) >= 2 && path[1] == ':' && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) {
		return strings.ToUpper(string(path[0])) + ":"
	}
	if strings.HasPrefix(path, `\\?\`) && len(path) >= 6 && path[5] == ':' {
		return strings.ToUpper(string(path[4])) + ":"
	}
	return ""
}

// parseLxssRegistryOutput parses reg.exe query output for the Lxss registry tree.
func parseLxssRegistryOutput(output string) (map[string]wslDistroInfo, string) {
	distros := make(map[string]wslDistroInfo)
	var defaultGUID string

	lines := strings.Split(output, "\n")
	var currentGUID string
	var currentDistro wslDistroInfo

	saveCurrent := func() {
		if currentDistro.DistributionName != "" || currentDistro.BasePath != "" {
			key := currentDistro.DistributionName
			if key == "" {
				key = currentGUID
			}
			if key != "" {
				distros[key] = currentDistro
			}
			if currentGUID != "" {
				distros[currentGUID] = currentDistro
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
		if len(fields) >= 3 {
			name := fields[0]
			valType := fields[1]
			val := strings.Join(fields[2:], " ")
			if valType == "REG_SZ" || valType == "REG_EXPAND_SZ" {
				switch strings.ToLower(name) {
				case "defaultdistribution":
					defaultGUID = strings.TrimSpace(val)
				case "distributionname":
					currentDistro.DistributionName = strings.TrimSpace(val)
				case "basepath":
					currentDistro.BasePath = strings.TrimSpace(val)
					currentDistro.DriveLetter = extractDriveLetter(val)
				}
			}
		}
	}
	saveCurrent()
	return distros, defaultGUID
}

// resolveDistroBackingDrive determines the host drive letter backing the WSL distro.
func resolveDistroBackingDrive(ctx context.Context) (string, error) {
	distroName := os.Getenv("WSL_DISTRO_NAME")
	out, err := wslRegistryQueryExecutor(ctx)
	if err != nil {
		if fallback := os.Getenv("SYSTEMDRIVE"); fallback != "" {
			if dl := extractDriveLetter(fallback); dl != "" {
				return dl, nil
			}
		}
		return "", fmt.Errorf("query lxss registry: %w", err)
	}

	distros, defaultGUID := parseLxssRegistryOutput(string(out))
	if len(distros) == 0 {
		return "", errors.New("no wsl distributions found in lxss registry")
	}

	// 1. Exact match on WSL_DISTRO_NAME
	if distroName != "" {
		if info, ok := distros[distroName]; ok && info.DriveLetter != "" {
			return info.DriveLetter, nil
		}
	}

	// 2. Default distribution GUID
	if defaultGUID != "" {
		if info, ok := distros[defaultGUID]; ok && info.DriveLetter != "" {
			return info.DriveLetter, nil
		}
	}

	// 3. Single registered distribution fallback
	if len(distros) == 1 {
		for _, info := range distros {
			if info.DriveLetter != "" {
				return info.DriveLetter, nil
			}
		}
	}

	// 4. Default to first valid drive letter if distroName is unset
	if distroName == "" {
		for _, info := range distros {
			if info.DriveLetter != "" {
				return info.DriveLetter, nil
			}
		}
	}

	return "", fmt.Errorf("unable to determine backing host drive for distro %q", distroName)
}

// findDriveMountPath locates the Linux mount point corresponding to a Windows drive letter.
func findDriveMountPath(driveLetter string, mountsData []byte) (string, error) {
	drivePrefix := strings.ToUpper(strings.TrimSuffix(driveLetter, ":"))
	if drivePrefix == "" {
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

		if fsType == "9p" || fsType == "drvfs" || strings.Contains(opts, "aname=drvfs") {
			devTrim := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(device, "\\"), ":"))
			if devTrim == drivePrefix {
				return mountPoint, nil
			}
			if strings.Contains(strings.ToUpper(opts), "PATH="+drivePrefix+":") {
				return mountPoint, nil
			}
			if strings.HasSuffix(strings.ToLower(mountPoint), "/"+strings.ToLower(drivePrefix)) {
				return mountPoint, nil
			}
		}
	}

	// Fallback to standard /mnt/<drive> if directory exists
	standardMount := "/mnt/" + strings.ToLower(drivePrefix)
	if fi, err := os.Stat(standardMount); err == nil && fi.IsDir() {
		return standardMount, nil
	}

	return "", fmt.Errorf("mount point for Windows drive %s not found in /proc/mounts", driveLetter)
}

// isPathOnDrvFS checks if a path is already located on a Windows drvfs/9p mount.
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

// boundWSLCapacity bounds guest filesystem capacity by the actual physical Windows host volume.
// If not running in WSL, it returns guestCap unchanged.
// If running in WSL, it queries the host volume backing the distro and caps FreeBytes.
// If backing host capacity is unavailable, it fails closed with an explicit error.
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

	hostCap, err := wslDriveStatFS(mountPath)
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
