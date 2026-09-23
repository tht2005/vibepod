// Package e2e drives real pods through the real binaries. These tests create
// user namespaces and install seccomp filters, so they exercise the one part
// of vibepod that cannot be unit tested: the kernel's cooperation.
package e2e

import (
	"bytes"
	"encoding/json"
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
				"\n  - local: "+workDir+
				"\nremote_tools:\n  - vp-only-tool\nexec:\n  default: pod\n"), 0o644); err != nil {
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
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir,
		// Stands in for the credentials a real pod holds: it is in the session
		// baseline, so it must never appear on a remote.
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

// --- M2: visibility and the in-pod control plane ---------------------------

// An agent inside a pod may look at anything and steer nothing. Without this
// the agent could re-route itself to a machine its directory would never have
// chosen — a capability nobody granted it.
func TestInPodControlPlaneIsReadOnlyForTheAgent(t *testing.T) {
	out, errOut, _ := inPod(t, "vpctl tree --json >/dev/null && echo READ_OK; "+
		"vpctl log >/dev/null && echo LOG_OK; "+
		"vpctl down e2e 2>&1; vpctl use vptest 2>&1")
	if !strings.Contains(out, "READ_OK") {
		t.Errorf("the agent could not read the tree: %q / %q", out, errOut)
	}
	if !strings.Contains(out, "LOG_OK") {
		t.Errorf("the agent could not read the log: %q / %q", out, errOut)
	}
	if !strings.Contains(out, `"down" is not permitted from inside a pod`) {
		t.Errorf("down was not refused from inside the pod: %q", out)
	}
	// use is the sharp edge: an agent that can pin its own executor grants
	// itself a machine. Without a terminal it must be refused.
	if !strings.Contains(out, "needs a terminal") {
		t.Errorf("use was not gated inside the pod: %q", out)
	}
}

func TestTreeJSONIsParseable(t *testing.T) {
	inPod(t, "true")
	out, _, code := vpctl(t, "tree", "--json", "e2e")
	if code != 0 {
		t.Fatalf("tree --json exit %d", code)
	}
	var tree struct {
		Pod    string `json:"pod"`
		Mounts []struct {
			At     string `json:"at"`
			Kind   string `json:"kind"`
			Target string `json:"target"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		t.Fatalf("tree --json is not valid JSON: %v\n%s", err, out)
	}
	if tree.Pod != "e2e" {
		t.Errorf("tree reported pod %q", tree.Pod)
	}
	if len(tree.Mounts) == 0 {
		t.Errorf("tree reported no mounts")
	}
}

// The log is the trust surface: for a tool whose pitch is "your agent runs
// commands on prod", what ran and where has to be provable.
func TestLogRecordsWhereEachCommandRan(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	marker := "log-probe-7b3"
	onRemote(t, "cd "+remoteSrv+" && /usr/bin/env sh -c 'exit 9'")
	onRemote(t, "cd "+workDir+" && /usr/bin/mkdir -p "+filepath.Join(workDir, marker))
	time.Sleep(600 * time.Millisecond) // let the exit poller notice

	out, _, code := vpctlIn(t, remoteDir, "log", "e2e-remote")
	if code != 0 {
		t.Fatalf("log exit %d", code)
	}
	if !strings.Contains(out, "vptest") {
		t.Errorf("log does not show the remote command:\n%s", out)
	}
	if !strings.Contains(out, "✗ 9") {
		t.Errorf("log lost a non-zero remote exit status:\n%s", out)
	}
	if !strings.Contains(out, "mkdir") || !strings.Contains(out, "pod") {
		t.Errorf("log does not show the pod-local command:\n%s", out)
	}
}

// The session's first program is the one that deadlocked: vpinit forks it and
// waits for its execve, while that execve waits for vpinit to bind a shim.
// A shell is never shimmed, so this has to start with something that is.
func TestSessionWhoseFirstProgramNeedsAShim(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, errOut, code := vpctl(t, "run", "--shim-all", "--",
			"/usr/bin/env", "true")
		if code != 0 {
			t.Errorf("exit %d: %s %s", code, out, errOut)
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("starting a session whose first program needs a shim deadlocked")
	}
}

// A shim is normally bind-mounted over a binary that already exists in the
// pod, so a tool that lives only on a remote has nothing to shadow. Naming it
// in remote_tools puts a shim ahead of PATH instead.
func TestRemoteOnlyToolIsRoutedAndExplainsItself(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	out, _, _ := onRemote(t, "cd "+workDir+" && ls -1 /vp/bin")
	if !strings.Contains(out, "vp-only-tool") {
		t.Fatalf("no shim was placed for the remote-only tool:\n%s", out)
	}
	// From a local directory there is no machine that has it, and saying
	// "not found" would send the reader looking in the wrong place.
	_, errOut, code := onRemote(t, "cd "+workDir+" && vp-only-tool")
	if code != 75 {
		t.Errorf("exit %d, want 75; stderr=%q", code, errOut)
	}
	if !strings.Contains(errOut, "exists only on a remote") {
		t.Errorf("the error does not explain itself: %q", errOut)
	}
}

// A remote directory must be reachable at exactly one path.
//
// The socket that carries vpsh's questions is bound in from the pod's runtime
// directory, and that bind is recursive — so binding the directory itself also
// carried in the FUSE mounts beneath it, and the pod's own root. A second path
// to the same files is not a cosmetic problem: /vp is forced to run locally,
// so commands there read the remote's files over FUSE while executing on the
// wrong machine, silently.
func TestARemoteMountHasExactlyOnePathInThePod(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	out, _, code := onRemote(t, "cd "+workDir+
		" && /usr/bin/grep -c fuse /proc/self/mountinfo; /usr/bin/ls /vp/run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "1" {
		t.Errorf("the pod has %s FUSE mounts, want 1:\n%s", lines[0], out)
	}
	rest := strings.Join(lines[1:], " ")
	if strings.Contains(rest, "mnt") || strings.Contains(rest, "root") {
		t.Errorf("/vp/run exposes more than the socket: %q", rest)
	}
	if !strings.Contains(rest, "pod.sock") {
		t.Errorf("/vp/run is missing the socket: %q", rest)
	}
}

// "Where can I go" is a question about machines, not paths, so it has a command
// of its own.
func TestHostsAndWhereNameMachinesAndTheirDirectories(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	out, _, code := vpctlIn(t, remoteDir, "hosts", "e2e-remote")
	if code != 0 {
		t.Fatalf("hosts exit %d: %s", code, out)
	}
	for _, want := range []string{"@pod", "@vptest", remoteSrv} {
		if !strings.Contains(out, want) {
			t.Errorf("hosts does not mention %q:\n%s", want, out)
		}
	}
	// Both readings of "where": a directory's machine, and a machine's
	// directory.
	out, _, code = vpctlIn(t, remoteDir, "where", "@vptest")
	if code != 0 || !strings.Contains(out, remoteSrv) {
		t.Errorf("where @vptest did not name its directory (exit %d): %s", code, out)
	}
	out, _, code = vpctlIn(t, remoteDir, "where", "-q", "@vptest")
	if code != 0 || strings.TrimSpace(out) != remoteSrv {
		t.Errorf("where -q must print a bare path for `cd $(...)`: %q", out)
	}
}

// A routed command must carry what the caller set for it, and nothing else.
//
// Dropping it entirely was the bug, and it failed silently: a discarded
// PYTHONPATH arrives as ModuleNotFoundError, which reads as a broken install
// rather than a discarded environment. Forwarding everything is not the fix —
// a pod's environment holds the credentials this design exists to keep on one
// machine, and describes this machine rather than the remote.
func TestARoutedCommandCarriesOnlyWhatTheCallerSet(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	// What the caller set for this command crosses.
	out, errOut, code := onRemote(t, "cd "+remoteSrv+
		" && VP_PROBE=hello /usr/bin/env | /usr/bin/grep '^VP_PROBE='")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "VP_PROBE=hello") {
		t.Errorf("the caller's variable did not reach the remote: %q", out)
	}

	// An exported variable is the same thing one step earlier.
	out, _, _ = onRemote(t, "cd "+remoteSrv+
		" && export VP_EXPORTED=yes && /usr/bin/env | /usr/bin/grep '^VP_EXPORTED='")
	if !strings.Contains(out, "VP_EXPORTED=yes") {
		t.Errorf("an exported variable did not reach the remote: %q", out)
	}

	// The remote's own identity is not overwritten with ours. The fixture host
	// is this machine, so compare against the session's value rather than a
	// name: what matters is that we did not impose it.
	out, _, _ = onRemote(t, "cd "+remoteSrv+" && /usr/bin/env | /usr/bin/grep '^PATH='")
	if strings.Contains(out, "/vp/bin") {
		t.Errorf("the pod's PATH was imposed on the remote: %q", out)
	}

	// And a variable that was already in the session baseline stays here. This
	// is the project's headline promise, so it gets a credential-shaped name.
	out, _, _ = onRemote(t, "cd "+remoteSrv+
		" && /usr/bin/env | /usr/bin/grep -c VP_FAKE_TOKEN || true")
	if strings.TrimSpace(out) != "0" {
		t.Errorf("a variable from the session baseline crossed to the remote: %q", out)
	}
}

// Refusing silently is how the original bug survived, so a variable we decline
// to forward has to be visible somewhere.
func TestARefusedVariableIsReported(t *testing.T) {
	if ssh == nil {
		t.Skip("no sshd fixture on this machine")
	}
	onRemote(t, "cd "+remoteSrv+" && PATH=/nonsense /usr/bin/true")
	time.Sleep(600 * time.Millisecond)
	out, _, _ := vpctlIn(t, remoteDir, "log", "e2e-remote")
	if !strings.Contains(out, "not forwarded: PATH") {
		t.Errorf("a refused variable was dropped silently:\n%s", out)
	}
}
