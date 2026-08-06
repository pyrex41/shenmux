package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TryOverlay attempts an unprivileged overlayfs mount in a new user+mount
// namespace via `unshare -Urm mount -t overlay ...`. The kernel here advertises
// overlay in /proc/filesystems, but privilege may still block the mount. This
// degrades gracefully: on any failure it returns used=false with a clear
// message and a nil error, because the checkpoint path already works by hashing
// the whole workspace, so overlay is an enhancement, not a requirement.
//
// It returns (used, message, err). err is non-nil only for programming-level
// problems (e.g. cannot create the target dirs), never for a blocked mount.
func TryOverlay(lower, upper, work, merged string) (bool, string, error) {
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return false, "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	if _, err := os.Stat(lower); err != nil {
		return false, "", fmt.Errorf("lowerdir %s: %w", lower, err)
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		return false, "unshare not found; falling back to whole-tree checkpoint", nil
	}

	opt := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	cmd := exec.Command("unshare", "-Urm",
		"mount", "-t", "overlay", "overlay", "-o", opt, merged)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("overlayfs mount unavailable (%v: %s); "+
			"falling back to whole-tree checkpoint",
			err, strings.TrimSpace(string(out)))
		return false, msg, nil
	}
	return true, "overlayfs mounted via `unshare -Urm` (upperdir delta path)", nil
}
