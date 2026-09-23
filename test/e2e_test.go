// Package e2e drives real pods through the real binaries. These tests create
// user namespaces and install seccomp filters, so they exercise the one part
// of vibepod that cannot be unit tested: the kernel's cooperation.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if err := os.Symlink("vibepod", filepath.Join(binDir, "vpctl")); err != nil {
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

	// A real remote, if this machine can host one. Tests that need it skip
	// when it is missing rather than failing for the wrong reason.
	ssh, err = startSSHD(filepath.Join(tmp, "sshd"), 2223)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: remote tests will be skipped:", err)
	} else {
		defer ssh.stop()
		remoteSrv = filepath.Join(tmp, "srv")
		if err := os.MkdirAll(remoteSrv, 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(remoteSrv, "data.txt"),
			[]byte("served from the remote\n"), 0o644); err != nil {
			return 0, err
		}
		remoteDir = filepath.Join(tmp, "remote-project")
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(remoteDir, "vibepod.yaml"), []byte(
			"pod: e2e-remote\nmounts:\n  - remote: vptest:"+remoteSrv+
				"\n  - local: "+workDir+"\nexec:\n  default: pod\n"), 0o644); err != nil {
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
	defer func() {
		_ = daemonP.Process.Kill()
		_, _ = daemonP.Process.Wait()
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

// vpctl runs the client the way a user would, from the project directory.
func vpctl(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return vpctlIn(t, projectDir, args...)
}

func vpctlIn(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "vpctl"), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir)
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
		t.Fatalf("vpctl %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// inPod runs a shell command inside the pod with every binary shimmed, which
// is the strongest form of the interception test.
func inPod(t *testing.T, script string) (string, string, int) {
	t.Helper()
	return vpctl(t, "run", "--shim-all", "--", "/bin/sh", "-c", script)
}

func TestPodRunsAndInterceptsEveryExec(t *testing.T) {
	out, errOut, code := inPod(t, "/usr/bin/id -u; /usr/bin/hexdump -n 4 -e '\"%d\"' /dev/zero")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	if !strings.HasPrefix(out, fmt.Sprint(os.Getuid())) {
		t.Errorf("pod should run as the invoking user, got %q", out)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("shimmed hexdump did not produce its real output: %q", out)
	}
}

// The shim must not change what a command sees. This is the property that a
// shell-level interception design could not hold.
func TestCwdSurvivesInterception(t *testing.T) {
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
			t.Errorf("exit code %d did not survive the shim, got %d", want, got)
		}
	}
}

// The pod is not a security boundary against a determined attacker, but it
// must contain the ordinary accident: an agent must not be able to unpick its
// own routing.
func TestPodProcessesHoldNoCapabilities(t *testing.T) {
	out, _, _ := inPod(t, "grep -E '^CapEff|^CapAmb' /proc/self/status")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasSuffix(strings.TrimSpace(line), "0000000000000000") {
			t.Errorf("pod process holds capabilities: %s", line)
		}
	}
}

func TestAgentCannotRemoveAShim(t *testing.T) {
	// /usr/bin/id is shimmed by the time this runs.
	out, _, _ := inPod(t, "/usr/bin/id -u >/dev/null; "+
		"/usr/bin/umount /usr/bin/id 2>&1 || true; "+
		"/usr/bin/mount --bind /bin/sh /usr/bin/id 2>&1 || true")
	if !strings.Contains(out, "permitted") && !strings.Contains(out, "superuser") &&
		!strings.Contains(out, "denied") {
		t.Errorf("expected the pod to refuse mount operations, got: %q", out)
	}
}

func TestPsListsTheRunningPod(t *testing.T) {
	inPod(t, "true")
	out, _, code := vpctl(t, "ps")
	if code != 0 {
		t.Fatalf("ps exit %d", code)
	}
	if !strings.Contains(out, "e2e") {
		t.Errorf("ps did not list the pod:\n%s", out)
	}
}

// --- M1: remotes -----------------------------------------------------------

// onRemote runs a shell command in the pod that has a remote mount.
func onRemote(t *testing.T, script string) (string, string, int) {
	t.Helper()
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	return vpctlIn(t, remoteDir, "run", "--", "/bin/sh", "-c", script)
}

// The whole premise: a command whose directory belongs to another machine
// runs there, without the agent knowing anything about it.
func TestCwdDecidesTheMachine(t *testing.T) {
	out, errOut, code := onRemote(t, "cd "+remoteSrv+" && /usr/bin/env | grep -c SSH_CONNECTION")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "1" {
		t.Errorf("command in a remote directory did not run over ssh: %q", out)
	}
}

// ...and the converse: a local directory stays in the pod. Without this, the
// routing rule would be "everything goes remote", which is not a rule.
func TestLocalDirectoryStaysInThePod(t *testing.T) {
	out, _, code := onRemote(t, "cd "+workDir+" && /usr/bin/env | grep -c SSH_CONNECTION || true")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "0" {
		t.Errorf("command in a local directory was shipped to a remote: %q", out)
	}
}

func TestRemoteFilesReadThroughTheMount(t *testing.T) {
	// Read it with a pod-local tool, so this exercises FUSE and not ssh.
	out, _, code := onRemote(t, "cd "+workDir+" && /usr/bin/cat "+
		filepath.Join(remoteSrv, "data.txt"))
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "served from the remote") {
		t.Errorf("mount did not serve the remote file: %q", out)
	}
}

// A routed command writes on the remote; the mount must show it afterwards.
// This is the case a cached network filesystem gets wrong, and the one
// vibepod can get right because it knows when the command finished.
func TestWritesByARoutedCommandAreVisible(t *testing.T) {
	name := "written-remotely.txt"
	_, errOut, code := onRemote(t, "cd "+remoteSrv+" && /usr/bin/tee "+name+" </dev/null")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	out, _, _ := onRemote(t, "cd "+workDir+" && /usr/bin/ls "+remoteSrv)
	if !strings.Contains(out, name) {
		t.Errorf("a file written by a routed command is not visible through the mount: %q", out)
	}
}

func TestRemoteExitCodeIsProxied(t *testing.T) {
	for _, want := range []int{0, 3, 42} {
		_, _, got := onRemote(t, fmt.Sprintf("cd %s && /usr/bin/env sh -c 'exit %d'",
			remoteSrv, want))
		if got != want {
			t.Errorf("remote exit %d came back as %d", want, got)
		}
	}
}
