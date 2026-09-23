package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M4: the config is a live object, not a boot artifact.
//
// This answers the complaint that shaped v2. Adding a machine used to mean
// `down`, edit the YAML, `up` — and losing every session, and the agent's
// context with them. A mount added an hour in has to be the same mount as one
// named in the config, or there are two implementations of the only thing this
// program does.

// livePod brings up a pod of its own so that mounting and unmounting cannot
// disturb the pods the other tests share.
func livePod(t *testing.T, name string) string {
	t.Helper()
	requireSSH(t)
	dir := t.TempDir()
	cfg := "pod: " + name + "\nmounts:\n  - remote: vptest:" + remoteSrv +
		"\n  - local: " + workDir + "\ncan_mount: [vptest, vptest2]\n" +
		"exec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := vpIn(t, dir, "up"); code != 0 {
		t.Fatalf("up: %s", errOut)
	}
	t.Cleanup(func() { vpIn(t, dir, "down", name) })
	return dir
}

// The headline: a machine appears in a running pod, and the session that was
// already there sees it.
func TestMountIntoARunningPod(t *testing.T) {
	dir := livePod(t, "e2e-live")
	out, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt)
	if code != 0 {
		t.Fatalf("mount: %s %s", out, errOut)
	}
	if !strings.Contains(out, remoteAlt) {
		t.Errorf("mount did not say where it landed: %q", out)
	}
	// The files are there, at the same absolute path, without a restart.
	out, errOut, code = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"/usr/bin/cat "+filepath.Join(remoteAlt, "second.txt"))
	if code != 0 {
		t.Fatalf("reading the new mount: %s %s", out, errOut)
	}
	if !strings.Contains(out, "from the second machine") {
		t.Errorf("the new mount does not serve its files: %q", out)
	}
	// And the machine is a backend now, which is the point of mounting it.
	out, _, _ = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /usr/bin/env | /usr/bin/grep -c SSH_CONNECTION")
	if strings.TrimSpace(out) != "1" {
		t.Errorf("the machine mounted at runtime is not usable as a backend: %q", out)
	}
	// The agent is told, without being restarted: the brief is regenerated from
	// the live mount list, and it is the only thing that tells an agent the
	// machine exists.
	out, _, _ = vpIn(t, dir, "brief", "e2e-live")
	if !strings.Contains(out, "vptest2") {
		t.Errorf("the agent brief does not mention the machine just mounted:\n%s", out)
	}
}

// A session that was open before the mount must not have to be restarted, which
// is the whole reason this is not `down` and `up`.
func TestAnOpenSessionSurvivesAMount(t *testing.T) {
	dir := livePod(t, "e2e-live-sess")
	r := onPTYIn(t, dir, "shell", "e2e-live-sess")
	defer r.stop()
	r.ready(t, 15*time.Second)
	r.send("VP_BEFORE=yes\n")

	if out, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s %s", out, errOut)
	}
	r.send("echo kep''t:$VP_BEFORE\n")
	if !r.waitFor(t, "kept:yes", 10*time.Second) {
		t.Errorf("the session did not survive the mount; got:\n%s", r.out.String())
	}
	r.send("/usr/bin/cat " + filepath.Join(remoteAlt, "second.txt") + "\n")
	if !r.waitFor(t, "from the second machine", 10*time.Second) {
		t.Errorf("the open session cannot see the new mount; got:\n%s", r.out.String())
	}
	r.send("exit\n")
}

func TestUnmountReleasesTheDirectoryAndTheBackend(t *testing.T) {
	dir := livePod(t, "e2e-unmount")
	if _, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s", errOut)
	}
	if out, errOut, code := vpIn(t, dir, "unmount", "@vptest2"); code != 0 {
		t.Fatalf("unmount: %s %s", out, errOut)
	}
	out, _, _ := vpIn(t, dir, "tree", "e2e-unmount")
	if strings.Contains(out, "vptest2") {
		t.Errorf("the tree still lists the unmounted machine:\n%s", out)
	}
	// The directory is gone from the pod, and the failure says so plainly rather
	// than serving a stale cache.
	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"/usr/bin/cat "+filepath.Join(remoteAlt, "second.txt"))
	if code == 0 {
		t.Errorf("the unmounted directory is still readable: %q", out)
	}
	_ = errOut
	// And the machine is no longer a backend.
	_, errOut, code = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp @vptest2 /usr/bin/true")
	if code == 0 {
		t.Errorf("an unmounted machine is still accepted as a backend")
	}
	if !strings.Contains(errOut, "vptest2") {
		t.Errorf("the refusal does not name the machine: %q", errOut)
	}
}

// Order decides shadowing, so runtime mounts are append-only: a mount that would
// sit above or below one already there changes what the existing one means,
// while work is running in it.
func TestMountRefusesToShadowSomethingAlreadyMounted(t *testing.T) {
	dir := livePod(t, "e2e-shadow")
	sub := filepath.Join(remoteSrv, "inner")
	_, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt, sub)
	if code == 0 {
		t.Fatal("a mount inside an existing mount was accepted")
	}
	if !strings.Contains(errOut, remoteSrv) || !strings.Contains(errOut, "shadow") {
		t.Errorf("the refusal does not name both paths: %q", errOut)
	}
}

// Two caches over the same bytes cannot be made coherent after the fact, so the
// overlap is refused rather than reported later as a mystery.
func TestMountRefusesOverlappingOrigins(t *testing.T) {
	dir := livePod(t, "e2e-overlap")
	_, errOut, code := vpIn(t, dir, "mount",
		"vptest:"+filepath.Join(remoteSrv, "sub"), "/elsewhere-in-the-pod")
	if code == 0 {
		t.Fatal("two mounts over the same files were accepted")
	}
	if !strings.Contains(errOut, "coherent") {
		t.Errorf("the refusal does not say why: %q", errOut)
	}
}

func TestMountRefusesToShadowASystemPath(t *testing.T) {
	dir := livePod(t, "e2e-sysguard")
	_, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt, "/usr")
	if code == 0 {
		t.Fatal("a mount over /usr was accepted")
	}
	if !strings.Contains(errOut, "/usr") {
		t.Errorf("the refusal does not name the path: %q", errOut)
	}
}

// Mounting is the one thing the in-pod control plane does not leave
// unrestricted, and it is a different kind of boundary from a destructive
// command: it opens a network path out of a sandbox built for containment.
func TestMountFromInsideThePodIsAllowlisted(t *testing.T) {
	dir := livePod(t, "e2e-allow")
	// Allowed: in can_mount.
	out, errOut, code := vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp mount vptest2:"+remoteAlt)
	if code != 0 {
		t.Fatalf("an allowlisted mount was refused from inside the pod: %s %s",
			out, errOut)
	}
	// Refused: not in can_mount, and the refusal says what would make it work.
	_, errOut, code = vpIn(t, dir, "run", "--", "/bin/sh", "-c",
		"vp mount nowhere-at-all:/tmp")
	if code == 0 {
		t.Fatal("a mount of an unlisted machine was accepted from inside the pod")
	}
	if !strings.Contains(errOut, "can_mount") {
		t.Errorf("the refusal does not say what to change: %q", errOut)
	}
}

// A session standing in a directory that is about to stop existing is worth
// refusing: the alternative is a shell whose cwd is gone, which reports itself
// as an unrelated failure several commands later.
func TestUnmountRefusesWhileASessionIsStandingThere(t *testing.T) {
	dir := livePod(t, "e2e-busy")
	if _, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s", errOut)
	}
	r := onPTYIn(t, dir, "shell", "e2e-busy")
	defer r.stop()
	r.ready(t, 15*time.Second)
	// The session's recorded directory is where it started, so start one there.
	r2 := onPTYIn(t, dir, "shell", "-C", remoteAlt, "e2e-busy")
	defer r2.stop()
	r2.ready(t, 15*time.Second)

	_, errOut, code := vpIn(t, dir, "unmount", "@vptest2")
	if code == 0 {
		t.Fatal("unmounted a directory a session was working in")
	}
	if !strings.Contains(errOut, "session") {
		t.Errorf("the refusal does not say which session: %q", errOut)
	}
	r.send("exit\n")
	r2.send("exit\n")
}

// `vp save` writes the file it would have read: the pod as it is actually
// running, including what was mounted after it started.
func TestSaveWritesTheLiveStateBack(t *testing.T) {
	dir := livePod(t, "e2e-save")
	if _, errOut, code := vpIn(t, dir, "mount", "vptest2:"+remoteAlt); code != 0 {
		t.Fatalf("mount: %s", errOut)
	}
	out := filepath.Join(dir, "saved.yaml")
	if _, errOut, code := vpIn(t, dir, "save", out); code != 0 {
		t.Fatalf("save: %s", errOut)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"pod: e2e-save", "vptest:" + remoteSrv,
		"vptest2:" + remoteAlt, workDir} {
		if !strings.Contains(text, want) {
			t.Errorf("the saved config lost %q:\n%s", want, text)
		}
	}
	// And it is a config: a pod started from it comes up the same way.
	saved := t.TempDir()
	text = strings.Replace(text, "pod: e2e-save", "pod: e2e-saved-again", 1)
	if err := os.WriteFile(filepath.Join(saved, "vibepod.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := vpIn(t, saved, "up"); code != 0 {
		t.Fatalf("a pod could not be started from the saved config: %s\n%s",
			errOut, text)
	}
	defer vpIn(t, saved, "down", "e2e-saved-again")
	// --json rather than the tree's own rendering, which shortens long paths to
	// fit a terminal.
	treeOut, _, _ := vpIn(t, saved, "tree", "--json", "e2e-saved-again")
	for _, want := range []string{remoteSrv, remoteAlt} {
		if !strings.Contains(treeOut, want) {
			t.Errorf("the reconstructed pod is missing %s:\n%s", want, treeOut)
		}
	}
}

// `vp hosts` answers "what else is there", which is what `vp mount` needs. A
// machine in the ssh config that this pod has not mounted is listed as such.
func TestHostsListsMachinesThisPodHasNotMounted(t *testing.T) {
	dir := livePod(t, "e2e-hostlist")
	out, _, code := vpIn(t, dir, "hosts", "e2e-hostlist")
	if code != 0 {
		t.Fatalf("hosts: %s", out)
	}
	if !strings.Contains(out, "vptest") {
		t.Errorf("hosts does not list the mounted machine:\n%s", out)
	}
	if !strings.Contains(out, "vptest2") || !strings.Contains(out, "not mounted") {
		t.Errorf("hosts does not offer the machine this pod could mount:\n%s", out)
	}
}

var _ = fmt.Sprint
