package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// M6a: the control plane.
//
// The composed tree changes while the pod runs, so every machine holding a copy of
// it has to converge — and one that cannot be reached must be left behind rather
// than block the change, and must catch up when it returns.

func controlProject(t *testing.T, name, extra string) string {
	t.Helper()
	requireSSH(t)
	dir := t.TempDir()
	cfg := "pod: " + name + "\nhosts:\n  vptest2: {}\n  vptest3: {}\n" +
		"mounts:\n  - remote: vptest:" + remoteSrv + "\n" + extra +
		"can_mount: [vptest, vptest2, vptest3]\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vpIn(t, dir, "down", name) })
	return dir
}

// A mount added to a running pod reaches the pods on other machines. Before the
// control plane, this combination was a refusal and a `down`/`up`.
func TestAMountAddedLaterReachesTheNodePods(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-add", "")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	if out, errOut, code := vpIn(t, dir, "mount", "vptest3:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s %s", out, errOut)
	}
	// The node pod holds it now, at the same path, and a command there can use it.
	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteAlt+" && vp @vptest2 /usr/bin/cat second.txt")
	if code != 0 || !strings.Contains(out, "from the second machine") {
		t.Fatalf("the node pod did not take the new mount (exit %d): %q %q",
			code, out, errOut)
	}
	out, _, _ = vpIn(t, dir, "node")
	if !strings.Contains(out, remoteAlt) {
		t.Errorf("`vp node` does not show the new mount on the node:\n%s", out)
	}
	if strings.Contains(out, "behind") {
		t.Errorf("the node is reported behind after converging:\n%s", out)
	}
}

// Unmounting reaches the node pods too: a node still holding a directory this pod
// no longer has would be divergence, not staleness.
func TestUnmountReachesTheNodePods(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-rm", "")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if _, errOut, code := vpIn(t, dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s", errOut)
	}
	if _, errOut, code := vpIn(t, dir, "mount", "vptest3:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "unmount", remoteAlt); code != 0 {
		t.Fatalf("unmount: %s %s", out, errOut)
	}
	out, _, _ := vpIn(t, dir, "node")
	if strings.Contains(out, remoteAlt) {
		t.Errorf("the node pod still holds a mount this pod released:\n%s", out)
	}
	out, _ = sshCapture(t, "vptest2", "grep -c "+remoteAlt+" /proc/mounts || true")
	if strings.TrimSpace(out) != "0" {
		t.Errorf("the node still has the mount in its namespace or its root: %q", out)
	}
}

// One write-back cache per mount. The first machine to take a writable mount keeps
// it; a second node pod gets it read-only and says so. That is the coherence vibepod
// can actually enforce — by mount flags, at mount time — since it is deliberately
// not in the data path and cannot see a file being opened.
func TestASecondNodeGetsAMountReadOnly(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-writer", "")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	for _, h := range []string{"vptest2", "vptest3"} {
		if out, errOut, code := vpIn(t, dir, "node", "add", h); code != 0 {
			t.Fatalf("node add %s: %s %s", h, out, errOut)
		}
	}
	out, _, _ := vpIn(t, dir, "node")
	if strings.Count(out, "read-only") != 1 {
		t.Errorf("expected exactly one node to hold the mount read-only:\n%s", out)
	}
	// And the read-only one really is: a write there fails rather than landing in a
	// second cache.
	_, e2, c2 := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/touch w2.txt")
	_, e3, c3 := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest3 /usr/bin/touch w3.txt")
	if (c2 == 0) == (c3 == 0) {
		for _, h := range []string{"vptest2", "vptest3"} {
			mi, _, _ := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
				"vp @"+h+" /usr/bin/grep srv /proc/self/mountinfo")
			t.Logf("%s mountinfo:\n%s", h, mi)
		}
		t.Errorf("expected exactly one of the two nodes to accept a write "+
			"(vptest2 exit %d: %q; vptest3 exit %d: %q)", c2, e2, c3, e3)
	}
}

// `writers: many` lifts that, for someone who knows the machines write different
// files and accepts that the last flush of any one file wins.
func TestManyWritersLetsEveryNodeWrite(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-many", "    writers: many\n")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	for _, h := range []string{"vptest2", "vptest3"} {
		if _, errOut, code := vpIn(t, dir, "node", "add", h); code != 0 {
			t.Fatalf("node add %s: %s", h, errOut)
		}
	}
	out, _, _ := vpIn(t, dir, "node")
	if strings.Contains(out, "read-only") {
		t.Errorf("writers: many still made a node read-only:\n%s", out)
	}
}

// A second daemon — this machine after a restart, or a laptop that lost its
// connection and came back — finds a pod already running on a node and adopts it
// instead of killing it. That is the whole reason a node pod outlives a disconnect.
func TestAPodAlreadyRunningOnANodeIsAdopted(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-adopt", "")
	other := startDaemon(t)
	if _, errOut, code := other.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up on the first daemon: %s", errOut)
	}
	if _, errOut, code := other.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add on the first daemon: %s", errOut)
	}
	// The first daemon goes away without taking the node pod with it.
	other.kill()

	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	_, errOut, code := vpIn(t, dir, "node", "add", "vptest2")
	if code != 0 {
		t.Fatalf("node add: %s", errOut)
	}
	if !strings.Contains(errOut, "adopting") {
		t.Errorf("the running node pod was rebuilt rather than adopted:\n%s", errOut)
	}
}

// A node pod whose daemon disappears flushes, unmounts and exits when its lease
// runs out. Dying with the ssh would kill a long run on a closed laptop lid;
// living forever would leave caches on a machine other people share.
func TestAnAbandonedNodePodLetsGoWhenItsLeaseRunsOut(t *testing.T) {
	dir := controlProject(t, "e2e-ctl-lease", "")
	// Short, so the test does not wait a day.
	cfg, _ := os.ReadFile(filepath.Join(dir, "vibepod.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"),
		append(cfg, []byte("lease: 2s\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	other := startDaemon(t)
	if _, errOut, code := other.vp(dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if _, errOut, code := other.vp(dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s", errOut)
	}
	// While the daemon lives, renewal keeps it alive past its lease.
	time.Sleep(3 * time.Second)
	if alive, _ := sshCapture(t, "vptest2",
		"test -S ~/.vp/run/e2e-ctl-lease@vptest2/node.sock && echo up || echo gone"); strings.TrimSpace(alive) != "up" {
		t.Fatalf("the node pod died while its daemon was still renewing: %q", alive)
	}
	other.kill()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := sshCapture(t, "vptest2",
			"test -S ~/.vp/run/e2e-ctl-lease@vptest2/node.sock && "+
				"~/.vp/bin/vibepod nodectl --pod e2e-ctl-lease@vptest2 <<< '{\"op\":\"ps\"}' "+
				">/dev/null 2>&1 && echo up || echo gone")
		if strings.TrimSpace(out) == "gone" {
			// And its state went with it: nothing will ever run `nodedown` for a pod
			// whose daemon has vanished.
			for i := 0; i < 20; i++ {
				left, _ := sshCapture(t, "vptest2",
					"test -e ~/.vp/run/e2e-ctl-lease@vptest2 && echo kept || echo gone")
				if strings.TrimSpace(left) == "gone" {
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
			t.Error("an expired node pod left its state behind")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("the node pod outlived its lease after its daemon went away")
}

// daemonRun is a second, independent daemon, for the tests about what happens when
// one goes away.
type daemonRun struct {
	t      *testing.T
	runDir string
	cmd    *exec.Cmd
}

func startDaemon(t *testing.T) *daemonRun {
	t.Helper()
	d := &daemonRun{t: t, runDir: t.TempDir()}
	d.cmd = exec.Command(filepath.Join(binDir, "vibepod"), "daemon", "-f")
	d.cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+d.runDir,
		"VIBEPOD_SSH_CONFIG="+ssh.configFile,
		"VIBEPOD_NODE_SSH_CONFIG="+ssh.configFile)
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitSock(filepath.Join(d.runDir, "host.sock")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.kill)
	return d
}

func (d *daemonRun) vp(dir string, args ...string) (string, string, int) {
	cmd := exec.Command(filepath.Join(binDir, "vp"), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+d.runDir,
		"VIBEPOD_SSH_CONFIG="+ssh.configFile)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// kill takes the daemon away the way a crash or a lost laptop would: without
// running down its pods, so node pods are left to fend for themselves.
func (d *daemonRun) kill() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	_, _ = d.cmd.Process.Wait()
	d.cmd = nil
	// A killed daemon leaves its own FUSE mounts behind — that is the crash being
	// simulated — so clear them the way the next daemon would, or the test's
	// temporary directory cannot be removed.
	b, _ := os.ReadFile("/proc/self/mountinfo")
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && strings.HasPrefix(f[4], d.runDir+"/") {
			_ = exec.Command("fusermount3", "-u", "-z", f[4]).Run()
			_ = exec.Command("umount", f[4]).Run()
		}
	}
}
