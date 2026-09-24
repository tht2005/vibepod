package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M6: every backend runs a pod.
//
// A machine with a node pod holds the composed zone at the same absolute paths as
// the pod on this machine. That is what makes "the data is on one machine, the
// compute is on another" work without translating anything — and what makes the
// same-string-different-bytes catastrophe of §6 inexpressible rather than merely
// unlikely.
//
// The fixture host is this machine reached over ssh, which is enough: pushing a
// binary, building a namespace there, mounting into it and dispatching through it
// are all the same operations they would be on a real node.

// nodeProject writes a config whose data lives on the fixture host, so a pod on
// that host owns it natively and a pod on the *other* alias has to cache it.
func nodeProject(t *testing.T, name string) string {
	t.Helper()
	requireSSH(t)
	dir := t.TempDir()
	cfg := "pod: " + name + "\nhosts:\n  vptest2: {}\nmounts:\n  - remote: vptest:" +
		remoteSrv + "\n    requires: [/usr/bin/env]\n    cache: 1G\n" +
		"exec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vpIn(t, dir, "down", name) })
	return dir
}

// A machine the config names gets its pod the first time a command is sent to
// it, so every command anywhere sees the same tree — its cwd, and every absolute
// path in its arguments. There is no plain-ssh fallback that would give those
// paths the bare machine's meaning.
func TestANodePodIsBuiltOnFirstUse(t *testing.T) {
	dir := nodeProject(t, "e2e-node-first")
	if _, errOut, code := vpIn(t, dir, "up"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	// A directory that belongs to vptest, dispatched to vptest2.
	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/cat data.txt")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "served from the remote") {
		t.Errorf("the node pod does not hold the composed zone: %q", out)
	}
	// From a directory that is nobody's, an absolute path in the arguments still
	// means the pod's: it is read inside vptest2's pod, from its mount.
	out, errOut, code = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd / && vp @vptest2 /usr/bin/grep -c "+remoteSrv+" /proc/self/mountinfo")
	if code != 0 || strings.TrimSpace(out) == "0" {
		t.Errorf("an absolute path was not the pod's on vptest2: %q %q", out, errOut)
	}
	// And the node keeps its own home, where its toolchain lives.
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /bin/sh -c 'test -d \"$HOME\" && echo home-ok'")
	if !strings.Contains(out, "home-ok") {
		t.Errorf("the node pod hides the machine's own home: %q", out)
	}
	out, _, _ = vpIn(t, dir, "node")
	if !strings.Contains(out, "vptest2") {
		t.Errorf("`vp node` does not list the pod built on first use:\n%s", out)
	}
}

// The headline: a pod on another machine, holding the same paths.
func TestANodePodReproducesThePathsAndKeepsItsOwnToolchain(t *testing.T) {
	dir := nodeProject(t, "e2e-node")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	out, errOut, code := vpIn(t, dir, "node", "add", "vptest2")
	if code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}

	// The composed zone is there, at the same path, with the same bytes.
	out, errOut, code = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/cat data.txt")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "served from the remote") {
		t.Errorf("the node pod does not hold the composed zone: %q", out)
	}
	// ...and the working directory is the one that was asked for, not a rewritten
	// private prefix. A traceback from over there is openable from here.
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest2 /usr/bin/pwd")
	if strings.TrimSpace(out) != remoteSrv {
		t.Errorf("the path was translated rather than reproduced: %q", out)
	}
	// The node's *own* system layer, not a minimal root: its toolchain is the
	// reason to dispatch there at all.
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /usr/bin/sh -c 'ls /usr/bin | wc -l'")
	if n := strings.TrimSpace(out); n == "" || n == "0" {
		t.Errorf("the node pod has no toolchain of its own: %q", out)
	}
	// And it is a pod, not the bare machine: the pod has a hostname of its own
	// and the mount is inside it.
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /usr/bin/grep -c fuse /proc/self/mountinfo")
	if strings.TrimSpace(out) == "0" {
		t.Errorf("the node pod is not holding its own mount: %q", out)
	}

	out, _, _ = vpIn(t, dir, "node")
	if !strings.Contains(out, "vptest2") || !strings.Contains(out, remoteSrv) {
		t.Errorf("`vp node` does not report what the node holds:\n%s", out)
	}
}

// A mount the node owns is a native bind: no FUSE, no cache, no round trip.
// Running work where the data lives is then full speed with nothing configured.
func TestANodeBindsWhatItOwnsNatively(t *testing.T) {
	dir := nodeProject(t, "e2e-node-native")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "node", "add", "vptest"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	out, _, _ := vpIn(t, dir, "node")
	if !strings.Contains(out, "its own") {
		t.Errorf("the node's own directory is not bound natively:\n%s", out)
	}
	// Not FUSE, which is the observable form of "native". (The node's own home
	// is in its pod too, with whatever the machine has mounted under it.)
	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest /usr/bin/grep ' "+remoteSrv+" ' /proc/self/mountinfo | /usr/bin/grep -c fuse || true")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "0" {
		t.Errorf("the machine that owns the data is reading it over FUSE: %q", out)
	}
	// And a write there is visible here, through the mount, straight away.
	name := "written-in-the-node-pod.txt"
	if _, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"cd "+remoteSrv+" && vp @vptest /usr/bin/tee "+name+" </dev/null"); code != 0 {
		t.Fatalf("writing in the node pod: %s", errOut)
	}
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c", "/usr/bin/ls "+remoteSrv)
	if !strings.Contains(out, name) {
		t.Errorf("a file written in the node pod is not visible here: %q", out)
	}
}

// requires: turns "is this machine actually equivalent?" into something `up`
// answers, rather than something a failed job tells you.
func TestRequiresIsCheckedBeforeAnythingIsBuilt(t *testing.T) {
	requireSSH(t)
	dir := t.TempDir()
	cfg := "pod: e2e-node-requires\nhosts:\n  vptest2: {}\nmounts:\n  - remote: vptest:" +
		remoteSrv + "\n    requires: [/opt/definitely-not-here]\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	defer vpIn(t, dir, "down", "e2e-node-requires")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	_, errOut, code := vpIn(t, dir, "node", "add", "vptest2")
	if code == 0 {
		t.Fatal("a pod was built on a machine missing what a mount requires")
	}
	if !strings.Contains(errOut, "/opt/definitely-not-here") {
		t.Errorf("the refusal does not name what is missing: %q", errOut)
	}
	if !strings.Contains(errOut, "requires") {
		t.Errorf("the refusal does not point at the config: %q", errOut)
	}
}

// An interactive session on a machine with a node pod is a shell *in that pod*,
// so the composed zone is there and `cd` means what it says.
func TestAShellOnANodeIsInsideItsPod(t *testing.T) {
	dir := nodeProject(t, "e2e-node-shell")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	r := onPTYIn(t, dir, "shell", "--raw", "-on", "vptest2", "-C", remoteSrv, "e2e-node-shell")
	defer r.stop()
	r.ready(t, 25*time.Second)
	r.send("pwd\n")
	if !r.waitFor(t, remoteSrv, 15*time.Second) {
		t.Errorf("the shell did not open in the composed zone; got:\n%s", r.out.String())
	}
	r.send("cat data.txt\n")
	if !r.waitFor(t, "served from the remote", 15*time.Second) {
		t.Errorf("the shell cannot read the composed zone; got:\n%s", r.out.String())
	}
	// cd persists, because it is a shell and not a sequence of them.
	r.send("cd / && pwd\n")
	if !r.waitFor(t, "\r\n/\r\n", 10*time.Second) {
		t.Logf("cd output was:\n%s", r.out.String())
	}
	r.send("exit\n")
}

// Dropping a node pod removes its state and brings its sessions home.
func TestNodeDropTakesTheMachineOutOfService(t *testing.T) {
	dir := nodeProject(t, "e2e-node-drop")
	if _, errOut, code := vpIn(t, dir, "up", "--push"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "node", "add", "vptest2"); code != 0 {
		t.Fatalf("node add: %s %s", out, errOut)
	}
	if out, errOut, code := vpIn(t, dir, "node", "drop", "vptest2"); code != 0 {
		t.Fatalf("node drop: %s %s", out, errOut)
	}
	out, _, _ := vpIn(t, dir, "node")
	if strings.Contains(out, "vptest2") {
		t.Errorf("the node pod is still listed:\n%s", out)
	}
	// And this pod's state is gone from that machine, not left behind holding
	// mounts. Other pods' state is none of this pod's business, so the check is
	// this pod's own directory.
	left, _ := sshCapture(t, "vptest2",
		"test -e ~/.vp/run/e2e-node-drop@vptest2 && echo kept || echo gone")
	if strings.TrimSpace(left) != "gone" {
		t.Errorf("the node kept this pod's state after it was dropped: %q", left)
	}
	// The binary stays: it is the footprint that was consented to, and re-pushing
	// it on the next `up` would cost a minute for nothing.
	bin, _ := sshCapture(t, "vptest2",
		"test -x ~/.vp/bin/vibepod && echo there || echo gone")
	if strings.TrimSpace(bin) != "there" {
		t.Errorf("dropping a node pod removed the binary too: %q", bin)
	}
}
