//go:build linux

package verifier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// ownershipCommand runs the existing lifecycle shell inside the supported
// Linux ownership bootstrap. bwrap owns user/PID/mount setup; the shell
// remains the lifecycle supervisor and keeps marker/handshake semantics.
func ownershipCommand(ctx context.Context, dir string, argv []string) (*exec.Cmd, error) {
	if os.Getenv(hermeticContainerEnv) == "1" {
		return exec.CommandContext(ctx, "sh", argv...), nil
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("ownership containment requires bwrap: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("ownership containment worktree path: %w", err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	bwrapArgs := []string{
		"--die-with-parent", "--as-pid-1", "--unshare-user", "--unshare-pid",
		"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid),
		// Keep the existing filesystem policy. Only namespace mounts change;
		// command paths, worktree, caches, and temporary files remain visible.
		"--bind", "/", "/", "--bind", absDir, absDir,
		"--proc", "/proc", "--dev", "/dev", "--chdir", absDir,
		"--info-fd", "6", "--",
		"sh",
	}
	if len(argv) >= 3 && argv[2] == "owned-wrap" {
		bwrapArgs = append(bwrapArgs, argv[:3]...)
		bwrapArgs = append(bwrapArgs, "--proc-ready")
		bwrapArgs = append(bwrapArgs, argv[3:]...)
	} else {
		bwrapArgs = append(bwrapArgs, argv...)
	}
	cmd := exec.CommandContext(ctx, bwrap, bwrapArgs...)
	cmd.Dir = absDir
	return cmd, nil
}

func ownershipInfoExpected() bool { return os.Getenv(hermeticContainerEnv) != "1" }
