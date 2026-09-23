package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The credential proxy. A node that has no key of its own reaches a third machine
// with this machine's ssh agent — but only when the config trusts that node with it.

// agentSetup starts an ssh-agent holding the fixture's key, and writes an ssh config
// for the node side with no key in it at all, so the agent is the only way in.
func agentSetup(t *testing.T) (agentSock, nodeCfg string) {
	t.Helper()
	requireSSH(t)
	dir := t.TempDir()
	agentSock = filepath.Join(dir, "agent.sock")
	out, err := exec.Command("ssh-agent", "-a", agentSock).CombinedOutput()
	if err != nil {
		t.Skipf("no ssh-agent: %v %s", err, out)
	}
	pid := ""
	for _, f := range strings.FieldsFunc(string(out), func(r rune) bool {
		return r == ';' || r == '\n' || r == ' '
	}) {
		if v, ok := strings.CutPrefix(f, "SSH_AGENT_PID="); ok {
			pid = v
		}
	}
	t.Cleanup(func() {
		if pid != "" {
			_ = exec.Command("kill", pid).Run()
		}
	})
	add := exec.Command("ssh-add", filepath.Join(ssh.dir, "id"))
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+agentSock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v %s", err, out)
	}
	cfg, _ := os.ReadFile(ssh.configFile)
	var kept []string
	for _, line := range strings.Split(string(cfg), "\n") {
		if strings.Contains(line, "IdentityFile") || strings.Contains(line, "IdentitiesOnly") {
			continue
		}
		kept = append(kept, line)
	}
	nodeCfg = filepath.Join(dir, "node_ssh_config")
	if err := os.WriteFile(nodeCfg, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return agentSock, nodeCfg
}

func agentProject(t *testing.T, name string, trust bool) string {
	t.Helper()
	dir := t.TempDir()
	trusted := "{}"
	if trust {
		trusted = "{forward_credentials: true}"
	}
	cfg := "pod: " + name + "\nhosts:\n  vptest2: " + trusted + "\nmounts:\n" +
		"  - remote: vptest:" + remoteSrv + "\n    via: direct\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestATrustedNodeReachesAThirdMachineWithTheForwardedAgent(t *testing.T) {
	sock, nodeCfg := agentSetup(t)
	d := startDaemon(t, "SSH_AUTH_SOCK="+sock, "VIBEPOD_NODE_SSH_CONFIG="+nodeCfg)
	dir := agentProject(t, "e2e-agent", true)
	defer d.vp(dir, "down", "e2e-agent")
	if _, errOut, code := d.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := d.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add with the agent: %s %s", out, errOut)
	}
	out, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/cat data.txt")
	if code != 0 || !strings.Contains(out, "served from the remote") {
		t.Fatalf("the node could not mount with the forwarded agent (exit %d): %q %q",
			code, out, errOut)
	}
	// A dispatched command there gets the agent too — `git push` from gpu03 is the
	// case — and it is vibepod's socket on that machine, not this one's.
	out, _, _ = d.vp(dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /bin/sh -c 'echo sock=$SSH_AUTH_SOCK; ssh-add -l'")
	if !strings.Contains(out, "sock=/vp/run/agent.sock") || !strings.Contains(out, "ED25519") {
		t.Errorf("a dispatched command did not get the forwarded agent: %q", out)
	}
}

// Untrusted is the default, and it means nothing is lent: the same node, the same
// mount, no agent — and so no way in.
func TestAnUntrustedNodeGetsNoAgent(t *testing.T) {
	sock, nodeCfg := agentSetup(t)
	d := startDaemon(t, "SSH_AUTH_SOCK="+sock, "VIBEPOD_NODE_SSH_CONFIG="+nodeCfg)
	dir := agentProject(t, "e2e-noagent", false)
	defer d.vp(dir, "down", "e2e-noagent")
	if _, errOut, code := d.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	_, errOut, code := d.vp(dir, "node", "add", "vptest2")
	if code == 0 {
		t.Fatal("a node with no key and no forwarded agent still reached the owner")
	}
	if !strings.Contains(errOut, "cannot reach") {
		t.Errorf("the refusal does not say what failed: %q", errOut)
	}
	out, _, _ := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /bin/sh -c 'echo sock=${SSH_AUTH_SOCK:-none}'")
	if !strings.Contains(out, "sock=none") {
		t.Errorf("an untrusted machine was given an agent socket: %q", out)
	}
}
