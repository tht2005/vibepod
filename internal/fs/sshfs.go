package fs

import (
	"fmt"
	"os/exec"
	"strings"
)

// Sshfs is the fallback backend: mature, present almost everywhere, and with
// no way to be told that its cache is stale. That last point is why it is the
// fallback — its timeouts have to stay short enough to be correct on their
// own, which costs round trips rclone would not pay.
type Sshfs struct{}

func (*Sshfs) Name() string { return "sshfs" }

func (*Sshfs) Mount(m *Mount, sshCommand string) error {
	opts := []string{
		"reconnect",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
		// Short attribute lifetimes: without an invalidation hook, correctness
		// has to come from not trusting the cache for long.
		"dir_cache=no",
		"attr_timeout=1",
		"entry_timeout=1",
		"ssh_command=" + sshCommand,
	}
	if m.ReadOnly {
		opts = append(opts, "ro")
	}
	cmd := exec.Command("sshfs", hostSpec(m), m.MountPoint, "-o", combine(opts))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (*Sshfs) Invalidate(*Mount, string) error { return ErrNoInvalidate }
