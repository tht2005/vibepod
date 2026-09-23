// Package e2e drives real pods through the real binaries. These tests create
// user namespaces, mount over sftp and open live shells on another machine, so
// they exercise the parts of vibepod that cannot be unit tested: the kernel's
// cooperation, and ssh's.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	binDir  string
	runDir  string
	daemonP *exec.Cmd
	workDir string
)

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	tmp, err := os.MkdirTemp("", "vibepod-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	binDir = filepath.Join(tmp, "bin")
	runDir = filepath.Join(tmp, "run")
	workDir = filepath.Join(tmp, "work")
	for _, d := range []string{binDir, runDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}
	for _, b := range []struct{ out, pkg string }{
		{"vibepod", "./cmd/vibepod"}, {"vpsh", "./cmd/vpsh"},
	} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, b.out), b.pkg)
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, fmt.Errorf("build %s: %v\n%s", b.pkg, err, out)
		}
	}
	if err := os.Symlink("vibepod", filepath.Join(binDir, "vp")); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(workDir, "README"),
		[]byte("hello from the pod\n"), 0o644); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "vibepod.yaml"), []byte(
		"pod: e2e\nmounts:\n  - local: "+workDir+"\nexec:\n  default: pod\n"), 0o644); err != nil {
		return 0, err
	}
	projectDir = tmp

	// A real remote, if this machine can host one. Tests that need it skip when
	// it is missing rather than failing for the wrong reason.
	ssh, err = startSSHD(filepath.Join(tmp, "sshd"), 2223)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: remote tests will be skipped:", err)
	} else {
		defer ssh.stop()
		remoteSrv = filepath.Join(tmp, "srv")
		remoteAlt = filepath.Join(tmp, "srv2")
		for _, d := range []string{remoteSrv, remoteAlt} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return 0, err
			}
		}
		if err := os.WriteFile(filepath.Join(remoteSrv, "data.txt"),
			[]byte("served from the remote\n"), 0o644); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(remoteAlt, "second.txt"),
			[]byte("from the second machine\n"), 0o644); err != nil {
			return 0, err
		}
		remoteDir = filepath.Join(tmp, "remote-project")
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			return 0, err
		}
		// One remote mount, one local directory, and a tool that exists only on
		// the remote. `exec.default: pod` because in v2 the machine is chosen:
		// a session starts here and goes elsewhere when told.
		if err := os.WriteFile(filepath.Join(remoteDir, "vibepod.yaml"), []byte(
			"pod: e2e-remote\nmounts:\n  - remote: vptest:"+remoteSrv+
				"\n  - local: "+workDir+
				"\nremote_tools:\n  vptest: [vp-only-tool]\n"+
				"can_mount: [vptest, vptest2]\n"+
				"exec:\n  default: pod\n"), 0o644); err != nil {
			return 0, err
		}
	}

	daemonP = exec.Command(filepath.Join(binDir, "vibepod"), "daemon", "-f")
	daemonP.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir)
	if ssh != nil {
		daemonP.Env = append(daemonP.Env, "VIBEPOD_SSH_CONFIG="+ssh.configFile)
	}
	daemonP.Stderr = os.Stderr
	if err := daemonP.Start(); err != nil {
		return 0, err
	}
	// Ask it to shut down rather than killing it: the daemon releases every FUSE
	// mount on its way out, and a killed one leaves them behind for whoever next
	// looks at /proc/mounts.
	defer func() {
		_ = daemonP.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = daemonP.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = daemonP.Process.Kill()
			_, _ = daemonP.Process.Wait()
		}
	}()
	if err := waitSock(filepath.Join(runDir, "host.sock")); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

var (
	projectDir string
	remoteDir  string // project whose mount lives on the fixture host
	remoteSrv  string // the directory the fixture serves
	remoteAlt  string // a second directory, for mounting at runtime
	ssh        *sshFixture
)

func waitSock(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("daemon socket %s never appeared", path)
}

// vp runs the client the way a user would, from the project directory.
func vp(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return vpIn(t, projectDir, args...)
}

func vpIn(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "vp"), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir,
		// Stands in for the credentials a real pod holds: it is in the session
		// baseline, so it must never appear on another machine.
		"VP_FAKE_TOKEN=shh")
	if ssh != nil {
		cmd.Env = append(cmd.Env, "VIBEPOD_SSH_CONFIG="+ssh.configFile)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("vp %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// inPod runs a shell command inside the local pod.
func inPod(t *testing.T, script string) (string, string, int) {
	t.Helper()
	return vp(t, "run", "--", "/bin/sh", "-c", script)
}

// inRemotePod runs a shell command inside the pod that has a remote mount. The
// shell itself always runs in the pod — that is the point of §3 — so anything
// that should happen elsewhere says so with `vp @`.
func inRemotePod(t *testing.T, script string) (string, string, int) {
	t.Helper()
	requireSSH(t)
	return vpIn(t, remoteDir, "run", "--", "/bin/sh", "-c", script)
}

func requireSSH(t *testing.T) {
	t.Helper()
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
}

// --- the pod itself --------------------------------------------------------

func TestPodRunsAsTheInvokingUserWithTheHostsToolchain(t *testing.T) {
	out, errOut, code := inPod(t, "/usr/bin/id -u; /usr/bin/hexdump -n 4 -e '\"%d\"' /dev/zero")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	if !strings.HasPrefix(out, fmt.Sprint(os.Getuid())) {
		t.Errorf("pod should run as the invoking user, got %q", out)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("the host's own tools are not usable in the pod: %q", out)
	}
}

// A path means the same thing inside the pod as outside it. Everything else in
// the design leans on this: it is what lets a traceback from another machine be
// opened here, and what makes moving a session between machines not move the
// ground under a prompt.
func TestAPathMeansTheSameThingInThePod(t *testing.T) {
	out, _, code := inPod(t, "cd "+workDir+" && /usr/bin/pwd && /usr/bin/cat README")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, workDir) {
		t.Errorf("cwd lost: %q", out)
	}
	if !strings.Contains(out, "hello from the pod") {
		t.Errorf("file not readable through the mount: %q", out)
	}
}

func TestExitCodeIsProxied(t *testing.T) {
	for _, want := range []int{0, 1, 42} {
		_, _, got := inPod(t, fmt.Sprintf("/usr/bin/env sh -c 'exit %d'", want))
		if got != want {
			t.Errorf("exit code %d did not survive the pod, got %d", want, got)
		}
	}
}

// The pod is not a security boundary against a determined attacker, but it must
// contain the ordinary accident: nothing inside it can alter its own mounts.
func TestPodProcessesHoldNoCapabilities(t *testing.T) {
	out, _, _ := inPod(t, "grep -E '^CapEff|^CapAmb' /proc/self/status")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasSuffix(strings.TrimSpace(line), "0000000000000000") {
			t.Errorf("pod process holds capabilities: %s", line)
		}
	}
}

func TestTheAgentCannotAlterAMount(t *testing.T) {
	out, _, _ := inPod(t, "/usr/bin/umount "+workDir+" 2>&1 || true; "+
		"/usr/bin/mount --bind /bin/sh "+workDir+" 2>&1 || true")
	if !strings.Contains(out, "permitted") && !strings.Contains(out, "superuser") &&
		!strings.Contains(out, "denied") && !strings.Contains(out, "root") {
		t.Errorf("expected the pod to refuse mount operations, got: %q", out)
	}
}

// /vp is the pod's own machinery and must be reachable at exactly one path.
//
// The socket that carries the pod's questions is bound in from the pod's runtime
// directory, and that bind is recursive — so binding the directory itself also
// carried in the FUSE mounts beneath it, and the pod's own root. A second path to
// the same files is not cosmetic: it is a command reading another machine's files
// from a directory nothing knows is remote.
func TestPodMachineryExposesOnlyWhatItMustNot(t *testing.T) {
	out, _, code := inRemotePod(t, "/usr/bin/grep -c fuse /proc/self/mountinfo; "+
		"/usr/bin/ls /vp/run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "1" {
		t.Errorf("the pod has %s FUSE mounts, want 1:\n%s", lines[0], out)
	}
	rest := strings.Join(lines[1:], " ")
	if strings.Contains(rest, "mnt") || strings.Contains(rest, "root") {
		t.Errorf("/vp/run exposes more than it should: %q", rest)
	}
	for _, want := range []string{"pod.sock", "brief.md"} {
		if !strings.Contains(rest, want) {
			t.Errorf("/vp/run is missing %s: %q", want, rest)
		}
	}
}

func TestPsListsTheRunningPodAndItsDefaultBackend(t *testing.T) {
	inPod(t, "true")
	out, _, code := vp(t, "ps")
	if code != 0 {
		t.Fatalf("ps exit %d", code)
	}
	if !strings.Contains(out, "e2e") {
		t.Errorf("ps did not list the pod:\n%s", out)
	}
	if !strings.Contains(out, "DEFAULT") || !strings.Contains(out, "pod") {
		t.Errorf("ps does not say which backend a new session opens on:\n%s", out)
	}
}

// Reading a remote file through the mount is the composition plane doing its job:
// the file is there, at its own absolute path, without anything being copied.
func TestRemoteFilesReadThroughTheMount(t *testing.T) {
	out, _, code := inRemotePod(t, "/usr/bin/cat "+
		filepath.Join(remoteSrv, "data.txt"))
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "served from the remote") {
		t.Errorf("the mount did not serve the remote file: %q", out)
	}
}

// A command that wrote on the other machine must be visible through the mount
// immediately afterwards. This is the case a cached network filesystem gets
// wrong and vibepod can get right, because it knows when the command finished.
func TestWritesByADispatchedCommandAreVisible(t *testing.T) {
	name := "written-remotely.txt"
	_, errOut, code := inRemotePod(t, "vp @vptest /usr/bin/tee "+
		filepath.Join(remoteSrv, name)+" </dev/null")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	out, _, _ := inRemotePod(t, "/usr/bin/ls "+remoteSrv)
	if !strings.Contains(out, name) {
		t.Errorf("a file written on the other machine is not visible through the "+
			"mount: %q", out)
	}
}

func TestTreeJSONIsParseable(t *testing.T) {
	inPod(t, "true")
	out, _, code := vp(t, "tree", "--json", "e2e")
	if code != 0 {
		t.Fatalf("tree --json exit %d", code)
	}
	var tree struct {
		Pod     string `json:"pod"`
		Default string `json:"default"`
		Mounts  []struct {
			At    string `json:"at"`
			Kind  string `json:"kind"`
			Owner string `json:"owner"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		t.Fatalf("tree --json is not valid JSON: %v\n%s", err, out)
	}
	if tree.Pod != "e2e" {
		t.Errorf("tree reported pod %q", tree.Pod)
	}
	if tree.Default == "" {
		t.Errorf("tree does not say which backend a session opens on:\n%s", out)
	}
	if len(tree.Mounts) == 0 {
		t.Errorf("tree reported no mounts")
	}
}

// Setting up a pod can mean waiting ten seconds on a machine that turns out not
// to exist. A wait that says nothing is indistinguishable from a hang, and an
// error at the end of it does not say which step it was about.
func TestPodSetupReportsWhatItIsWaitingFor(t *testing.T) {
	requireSSH(t)
	vpIn(t, remoteDir, "down", "e2e-remote")
	_, errOut, code := vpIn(t, remoteDir, "up")
	if code != 0 {
		t.Fatalf("up exit %d: %s", code, errOut)
	}
	for _, want := range []string{"connecting to vptest", "connected", "mounting", "mounted"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("setup never reported %q:\n%s", want, errOut)
		}
	}
}

// And when it fails, it must say which step failed and why — not the raw ssh
// text, which is written for someone debugging ssh rather than someone who asked
// for a pod.
func TestUnreachableHostSaysWhichStepAndWhy(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "w")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "pod: e2e-noaddr\nmounts:\n  - remote: no-such-host-at-all:/tmp\n" +
		"  - local: " + work + "\nexec:\n  default: pod\n"
	if err := os.WriteFile(filepath.Join(dir, "vibepod.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := vpIn(t, dir, "up")
	if code == 0 {
		t.Fatalf("up succeeded against a host that does not exist")
	}
	if !strings.Contains(errOut, "connecting to no-such-host-at-all") {
		t.Errorf("the failing step was not named:\n%s", errOut)
	}
	if !strings.Contains(errOut, "failed") {
		t.Errorf("the step was not marked failed:\n%s", errOut)
	}
	if !strings.Contains(errOut, "no address") || !strings.Contains(errOut, "ssh/config") {
		t.Errorf("the reason does not say what to check:\n%s", errOut)
	}
	// And not twice: the step says which, the error says why.
	if strings.Count(errOut, "no address") != 1 {
		t.Errorf("the reason was repeated:\n%s", errOut)
	}
}
