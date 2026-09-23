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
	target := hostSpec(m)
	if e := m.Endpoint; e != nil {
		// A relay: a loopback port tunnelled to a server this pod started, with a
		// key made for that one tunnel. The host key is the server's own, reached
		// through an authenticated ssh connection vibepod already holds, so there is
		// nothing a known_hosts entry would add.
		target = fmt.Sprintf("%s@%s:%s", e.User, e.Host, m.RemotePath)
		for i, o := range opts {
			if strings.HasPrefix(o, "ssh_command=") {
				opts[i] = "ssh_command=ssh -p " + fmt.Sprint(e.Port) + " -i " + e.KeyFile +
					" -o BatchMode=yes -o LogLevel=ERROR -o IdentitiesOnly=yes" +
					" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
			}
		}
	}
	cmd := exec.Command("sshfs", target, m.MountPoint, "-o", combine(opts))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (*Sshfs) Invalidate(*Mount, string) error { return ErrNoInvalidate }
