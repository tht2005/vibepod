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
	// enter, when set, is the command that puts what follows inside this
	// machine's node pod. See InPod.
	enter string
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

// BaseOpts are the options every vibepod ssh uses, wherever it runs — including
// the ones a *node* makes for itself, which is why they are exported.
//
// BatchMode means a machine that wants a passphrase fails immediately instead of
// waiting on a prompt nobody can see; on a node there is nobody at all, so a
// prompt there is a hang with no symptom. LogLevel=ERROR keeps ssh from talking
// about itself in the middle of someone's output — without it every command that
// takes a terminal ends with "Shared connection to host closed.", and a mount
// helper reading that stream treats it as damage.
func BaseOpts() []string {
	return []string{"-o", "BatchMode=yes", "-o", "LogLevel=ERROR"}
}

// Opts are the ssh options every invocation from this machine shares.
func (h *Host) Opts() []string {
	opts := []string{}
	// Hosts are ssh_config aliases. VIBEPOD_SSH_CONFIG points at a different
	// file than the user's own, which tests need and multi-account setups want.
	if f := os.Getenv("VIBEPOD_SSH_CONFIG"); f != "" {
		opts = append(opts, "-F", f)
	}
	opts = append(opts,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+h.ctlPath,
		"-o", "ControlPersist=300",
	)
	return append(opts, BaseOpts()...)
}

// SSHCommand is the ssh invocation sshfs and rclone should reuse, so mounts
// ride the same multiplexed connection as commands.
func (h *Host) SSHCommand() string {
	return "ssh " + strings.Join(h.Opts(), " ")
}

// ConnectTimeout bounds how long a host gets to answer before vibepod gives up
// on it. Long enough for a slow link, short enough that a dead one does not
// look like a hang.
const ConnectTimeout = 10

// Warm opens the master connection so the first real command does not pay for
// the handshake, and so an unreachable host is reported at up time — where a
// person is watching — rather than in the middle of an agent run.
func (h *Host) Warm() error {
	args := append(h.Opts(),
		"-o", fmt.Sprintf("ConnectTimeout=%d", ConnectTimeout), h.Alias, "true")
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", Explain(h.Alias, string(out)))
	}
	return nil
}

// explain turns ssh's output into a sentence that says what to do about it.
//
// The raw text is written for someone debugging ssh, not for someone who asked
// for a pod and got a ten-second pause. The distinctions that matter here are
// which of these it was, because each has a different next step.
func Explain(alias, out string) string {
	line := firstUseful(out)
	low := strings.ToLower(line)
	switch {
	case strings.Contains(low, "timed out"), strings.Contains(low, "timeout"):
		return fmt.Sprintf("%s timed out after %ds — unreachable, or behind a "+
			"jump host that is not in your ssh config", alias, ConnectTimeout)
	case strings.Contains(low, "could not resolve hostname"):
		return fmt.Sprintf("%s has no address — check the Host entry in "+
			"~/.ssh/config", alias)
	case strings.Contains(low, "connection refused"):
		return fmt.Sprintf("%s refused the connection — nothing is listening on "+
			"that port", alias)
	case strings.Contains(low, "permission denied"),
		strings.Contains(low, "no such identity"),
		strings.Contains(low, "authentication"):
		return fmt.Sprintf("%s rejected the key — vibepod runs ssh in batch "+
			"mode, so the key must already be in your agent (ssh-add) and not "+
			"need a passphrase", alias)
	case strings.Contains(low, "host key verification failed"):
		return fmt.Sprintf("%s failed host key verification — connect to it once "+
			"by hand to record the key", alias)
	case line == "":
		return fmt.Sprintf("%s could not be reached", alias)
	}
	return fmt.Sprintf("%s: %s", alias, line)
}

// firstUseful skips ssh's banners and warnings to reach the reason.
func firstUseful(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Warning: Permanently added") {
			continue
		}
		return line
	}
	return ""
}

// Reachable reports whether the multiplexed master is currently up.
func (h *Host) Reachable() bool {
	return exec.Command("ssh", "-o", "ControlPath="+h.ctlPath, "-O", "check",
		h.Alias).Run() == nil
}
