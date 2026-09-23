// Package remote runs commands on other machines.
//
// Every connection is an ssh_config alias, so ProxyJump, keys, ports and
// forwarding are inherited from the user's own configuration rather than
// reimplemented. Connections are multiplexed: the first command pays for the
// handshake, the rest cost a round trip.
//
// Private keys never enter a pod. The daemon runs outside every namespace and
// owns all of this.
package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ExitLinkDown is EX_TEMPFAIL, reserved for "vibepod could not place or
// complete this command". A partially-applied deploy must never be retried
// behind the agent's back, so a dropped link fails visibly instead.
const ExitLinkDown = 75

type Pool struct {
	ctlDir string

	mu    sync.Mutex
	hosts map[string]*Host
}

type Host struct {
	Alias   string
	ctlPath string
}

func NewPool(ctlDir string) (*Pool, error) {
	if err := os.MkdirAll(ctlDir, 0o700); err != nil {
		return nil, err
	}
	return &Pool{ctlDir: ctlDir, hosts: map[string]*Host{}}, nil
}

func (p *Pool) Host(alias string) *Host {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[alias]; ok {
		return h
	}
	h := &Host{Alias: alias, ctlPath: filepath.Join(p.ctlDir, alias+".ctl")}
	p.hosts[alias] = h
	return h
}

// Hosts lists the aliases this pool has touched.
func (p *Pool) Hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.hosts))
	for a := range p.hosts {
		out = append(out, a)
	}
	return out
}

// Close tears down every multiplexed master.
func (p *Pool) Close() {
	p.mu.Lock()
	hosts := make([]*Host, 0, len(p.hosts))
	for _, h := range p.hosts {
		hosts = append(hosts, h)
	}
	p.mu.Unlock()
	for _, h := range hosts {
		_ = exec.Command("ssh", "-o", "ControlPath="+h.ctlPath, "-O", "exit", h.Alias).Run()
	}
}

// Opts are the ssh options every invocation shares. BatchMode means a host
// that needs a passphrase fails immediately instead of hanging the exec gate
// on a prompt no one can see.
func (h *Host) Opts() []string {
	opts := []string{}
	// Hosts are ssh_config aliases. VIBEPOD_SSH_CONFIG points at a different
	// file than the user's own, which tests need and multi-account setups want.
	if f := os.Getenv("VIBEPOD_SSH_CONFIG"); f != "" {
		opts = append(opts, "-F", f)
	}
	return append(opts,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + h.ctlPath,
		"-o", "ControlPersist=300",
		"-o", "BatchMode=yes",
	)
}

// SSHCommand is the ssh invocation sshfs and rclone should reuse, so mounts
// ride the same multiplexed connection as commands.
func (h *Host) SSHCommand() string {
	return "ssh " + strings.Join(h.Opts(), " ")
}

// Warm opens the master connection so the first real command does not pay for
// the handshake, and so an unreachable host is reported at up time.
func (h *Host) Warm() error {
	args := append(h.Opts(), "-o", "ConnectTimeout=10", h.Alias, "true")
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s unreachable: %v: %s", h.Alias, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// Reachable reports whether the multiplexed master is currently up.
func (h *Host) Reachable() bool {
	return exec.Command("ssh", "-o", "ControlPath="+h.ctlPath, "-O", "check",
		h.Alias).Run() == nil
}
