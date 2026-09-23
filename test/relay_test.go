package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Relaying: a node reaching bytes through this machine, for a directory of this
// machine's own (reverse mounts) and for a node that cannot reach the owner.
//
// This side serves with rclone, which this machine may not have; the test uses one
// named by VIBEPOD_TEST_RCLONE and skips without it.

func relayDaemon(t *testing.T, extra ...string) *daemonRun {
	t.Helper()
	requireSSH(t)
	r := os.Getenv("VIBEPOD_TEST_RCLONE")
	if r == "" {
		t.Skip("set VIBEPOD_TEST_RCLONE to an rclone binary to test relaying")
	}
	return startDaemon(t, append([]string{"VIBEPOD_RCLONE=" + r}, extra...)...)
}

func relayProject(t *testing.T, name, mounts string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := "pod: " + name + "\nhosts:\n  vptest2: {}\nmounts:\n" + mounts +
		"exec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A directory on this machine, exposed to a node, is in that node's pod at the same
// path — which is "edit here, run there".
func TestALocalDirectoryExposedToANodeIsRelayed(t *testing.T) {
	d := relayDaemon(t)
	notExposed := t.TempDir()
	dir := relayProject(t, "e2e-rel-local",
		"  - local: "+workDir+"\n    expose_to: [vptest2]\n  - local: "+notExposed+"\n")
	defer d.vp(dir, "down", "e2e-rel-local")
	if _, errOut, code := d.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := d.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	out, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"cd "+workDir+" && vp @vptest2 /usr/bin/cat README && vp @vptest2 /usr/bin/pwd")
	if code != 0 || !strings.Contains(out, "hello from the pod") || !strings.Contains(out, workDir) {
		t.Fatalf("the exposed directory is not in the node pod at its own path "+
			"(exit %d): %q %q", code, out, errOut)
	}
	out, _, _ = d.vp(dir, "node")
	if !strings.Contains(out, "relayed") {
		t.Errorf("`vp node` does not say the directory is relayed:\n%s", out)
	}
	// Not exposed means not there: local files go to a remote machine only when the
	// config says so.
	if strings.Contains(out, notExposed) {
		t.Errorf("a directory that was not exposed reached the node:\n%s", out)
	}
	// And a write over there lands here.
	if _, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"cd "+workDir+" && vp @vptest2 /bin/sh -c 'echo from-the-node > written-remotely'"); code != 0 {
		t.Fatalf("write through the relay: %s", errOut)
	}
	d.vp(dir, "node", "drop", "vptest2") // drop flushes
	if b, err := os.ReadFile(filepath.Join(workDir, "written-remotely")); err != nil ||
		!strings.Contains(string(b), "from-the-node") {
		t.Errorf("a write in the node pod did not land in this machine's directory: %v %q", err, b)
	}
	os.Remove(filepath.Join(workDir, "written-remotely"))
}

// `via: relay` sends another machine's mount through this one.
func TestViaRelayCarriesAnotherMachinesMount(t *testing.T) {
	d := relayDaemon(t)
	dir := relayProject(t, "e2e-rel-via",
		"  - remote: vptest:"+remoteSrv+"\n    via: relay\n")
	defer d.vp(dir, "down", "e2e-rel-via")
	if _, errOut, code := d.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := d.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	out, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/cat data.txt")
	if code != 0 || !strings.Contains(out, "served from the remote") {
		t.Fatalf("the relayed mount is not readable on the node (exit %d): %q %q", code, out, errOut)
	}
	out, _, _ = d.vp(dir, "node")
	if !strings.Contains(out, "relayed through this machine") {
		t.Errorf("`vp node` does not say how the mount arrives:\n%s", out)
	}
}

// `via: auto` asks the node whether it can reach the owner by itself, and relays —
// saying why — when it cannot. Here the node's ssh config has no entry for the
// owner, which is what a firewalled cluster looks like from the node's side.
func TestAutoRelaysWhenTheNodeCannotReachTheOwner(t *testing.T) {
	cfg, err := os.ReadFile(ssh.configFile)
	if err != nil {
		t.Fatal(err)
	}
	// The node's view: vptest2 and vptest3, but no vptest.
	var kept []string
	for _, block := range strings.Split(string(cfg), "Host ") {
		if block != "" && !strings.HasPrefix(block, "vptest\n") {
			kept = append(kept, "Host "+block)
		}
	}
	nodeCfg := filepath.Join(t.TempDir(), "node_ssh_config")
	if err := os.WriteFile(nodeCfg, []byte(strings.Join(kept, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	d := relayDaemon(t, "VIBEPOD_NODE_SSH_CONFIG="+nodeCfg)
	dir := relayProject(t, "e2e-rel-auto", "  - remote: vptest:"+remoteSrv+"\n")
	defer d.vp(dir, "down", "e2e-rel-auto")
	if _, errOut, code := d.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := d.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	out, errOut, code := d.vp(dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/cat data.txt")
	if code != 0 || !strings.Contains(out, "served from the remote") {
		t.Fatalf("auto did not fall back to the relay (exit %d): %q %q", code, out, errOut)
	}
	log, _, _ := d.vp(dir, "log", "e2e-rel-auto")
	if !strings.Contains(log, "relaying through this machine") {
		t.Errorf("the fallback was not explained:\n%s", log)
	}
}

// Without rclone here there is nothing to serve a relay with, and the refusal says
// so rather than failing somewhere less obvious.
func TestRelayWithoutRcloneSaysSo(t *testing.T) {
	requireSSH(t)
	dir := relayProject(t, "e2e-rel-none",
		"  - local: "+workDir+"\n    expose_to: [vptest2]\n")
	defer vpIn(t, dir, "down", "e2e-rel-none")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	_, errOut, code := vpIn(t, dir, "node", "add", "vptest2")
	if code == 0 {
		t.Skip("this machine has rclone on PATH, so the relay worked")
	}
	if !strings.Contains(errOut, "rclone") {
		t.Errorf("the refusal does not name what is missing: %q", errOut)
	}
}
