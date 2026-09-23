package sys

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockedFlags are the mount options a user namespace may not clear. A mount
// inherited into an unprivileged namespace carries them locked, so a remount
// that omits any of them is refused with EPERM — which is why a read-only
// remount has to read the current options back first rather than simply
// asking for MS_RDONLY.
var lockedFlags = map[string]uintptr{
	"nosuid":     syscall.MS_NOSUID,
	"nodev":      syscall.MS_NODEV,
	"noexec":     syscall.MS_NOEXEC,
	"noatime":    syscall.MS_NOATIME,
	"nodiratime": syscall.MS_NODIRATIME,
	"relatime":   syscall.MS_RELATIME,
}

// currentFlags reports the options in force on the mount that covers path.
func currentFlags(path string) uintptr {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	var best string
	var flags uintptr
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		point, opts := fields[4], fields[5]
		// The most specific mount point covering path is the one in force.
		if point != abs && !strings.HasPrefix(abs, strings.TrimSuffix(point, "/")+"/") {
			continue
		}
		if len(point) < len(best) {
			continue
		}
		best = point
		flags = 0
		for _, o := range strings.Split(opts, ",") {
			if bit, ok := lockedFlags[o]; ok {
				flags |= bit
			}
		}
	}
	return flags
}

// RemountReadOnly makes an existing bind mount read-only while preserving
// every option the kernel will not let us drop.
func RemountReadOnly(dst string) error {
	flags := currentFlags(dst) | MsBind | MsRemount | MsRdonly
	return Mount("", dst, "", flags, "")
}
