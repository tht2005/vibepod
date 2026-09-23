package remote

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Relaying: a node reaching bytes through this machine.
//
// Two cases need it. A node that cannot reach the machine that owns a directory —
// node-to-node ssh is firewalled on many clusters — and a directory on *this*
// machine, which no node can reach at all. Both are the same mechanism: this
// machine serves the directory on its own loopback, and a reverse forward on the
// ssh connection vibepod already holds makes it a port on the node's loopback.
// Nothing listens on a network interface, and nothing survives the connection.

// ReverseForward makes a port on this machine's loopback reachable as a port on
// that machine's loopback, and returns the port the far side allocated.
func (h *Host) ReverseForward(local int) (int, error) {
	spec := fmt.Sprintf("0:127.0.0.1:%d", local)
	args := append(h.Opts(), "-O", "forward", "-R", spec, h.Alias)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("reverse forward to %s: %s", h.Alias,
			strings.TrimSpace(string(out)))
	}
	// ssh prints the allocated port, alone or in a sentence depending on version.
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 && n < 65536 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s allocated a port but did not say which: %q", h.Alias,
		strings.TrimSpace(string(out)))
}

// CancelReverse closes one. Best effort, like every teardown over a connection
// that may already be gone.
func (h *Host) CancelReverse(remote, local int) {
	spec := fmt.Sprintf("%d:127.0.0.1:%d", remote, local)
	args := append(h.Opts(), "-O", "cancel", "-R", spec, h.Alias)
	_ = exec.Command("ssh", args...).Run()
}

// CanReach asks a machine whether it can ssh to another by itself, with its own
// config and keys. That is what `via: direct` depends on, and the question is put
// to the node rather than answered here, because only the node knows.
func (h *Host) CanReach(target string, extra []string, agentSock string) error {
	cmd := "ssh " + strings.Join(append(BaseOpts(), extra...), " ") +
		" -o ConnectTimeout=5 " + quote(target) + " true"
	if agentSock != "" {
		cmd = "SSH_AUTH_SOCK=" + quote(agentSock) + " " + cmd
	}
	args := append(h.Opts(), h.Alias, cmd)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", Explain(target, string(out)))
	}
	return nil
}

// ForwardAgent makes this machine's ssh agent reachable on that machine at a unix
// socket path, over the multiplexed connection. The agent signs; no key is copied.
// The socket exists only while the connection does.
//
// Removing any old socket first matters: sshd refuses to bind over a path that
// exists unless its own StreamLocalBindUnlink is on, and that is off by default.
func (h *Host) ForwardAgent(remoteSock, localSock string) error {
	rm := append(h.Opts(), h.Alias, "rm -f "+quote(remoteSock)+"; mkdir -p "+
		quote(dirOf(remoteSock)))
	_ = exec.Command("ssh", rm...).Run()
	args := append(h.Opts(), "-O", "forward", "-R", remoteSock+":"+localSock, h.Alias)
	if out, err := exec.Command("ssh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("forward the ssh agent to %s: %s", h.Alias,
			strings.TrimSpace(string(out)))
	}
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
