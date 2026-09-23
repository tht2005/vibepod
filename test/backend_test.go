package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M3: the machine is chosen, not guessed.
//
// v1 inferred it from the working directory, through a seccomp gate on execve.
// These tests cover what replaced it: a shell that records, a verb that
// dispatches, and one backend per session.

// --- the dispatching shell -------------------------------------------------

// The pod's $SHELL records every command line it is asked to run. This is the
// observation half of what the exec gate did, and the whole of what vpsh is.
func TestTheShellRecordsEveryCommandItRuns(t *testing.T) {
	marker := "shell-probe-" + fmt.Sprint(time.Now().UnixNano()%100000)
	out, errOut, code := inPod(t, `$SHELL -c 'echo `+marker+`'`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, marker) {
		t.Fatalf("the command did not run: %q", out)
	}
	waitForLog(t, "e2e", marker, 3*time.Second)
	log, _, _ := vp(t, "log", "e2e")
	if !strings.Contains(log, marker) {
		t.Errorf("the shell did not record the command:\n%s", log)
	}
	if !strings.Contains(log, "pod") {
		t.Errorf("the log does not say the command ran in the pod:\n%s", log)
	}
}

// vpsh must not change what the command sees, because it is on the front of every
// command an agent runs. Exit status, arguments and stdio all pass through: it
// execs the user's real shell in place rather than interpreting anything.
func TestTheShellChangesNothingAboutTheCommand(t *testing.T) {
	for _, want := range []int{0, 3, 42} {
		_, _, got := inPod(t, fmt.Sprintf(`$SHELL -c 'exit %d'`, want))
		if got != want {
			t.Errorf("exit %d came back as %d through the pod's $SHELL", want, got)
		}
	}
	out, _, _ := inPod(t, `$SHELL -c 'printf a; printf b >&2' 2>/dev/null`)
	if out != "a" {
		t.Errorf("the pod's $SHELL merged or mangled the streams: %q", out)
	}
	// And it is the user's own shell, not a reimplementation of one: the daemon
	// hands it over in the environment precisely so that an agent's generated
	// wrapper keeps working.
	out, _, _ = inPod(t, `echo "$VIBEPOD_REAL_SHELL"`)
	if strings.TrimSpace(out) == "" || strings.Contains(out, "/vp/") {
		t.Errorf("VIBEPOD_REAL_SHELL is not a real shell: %q", out)
	}
}

// An agent does not run bare commands. Measured, Claude Code runs every command
// as a zsh wrapper that sources an environment snapshot, sets options, evals the
// command and writes the new cwd to a temp file. The log has to be readable
// anyway, so the renderer condenses the shape it knows and prints the rest.
func TestAnAgentsShellWrapperIsRecordedReadably(t *testing.T) {
	marker := "wrapped-probe-" + fmt.Sprint(time.Now().UnixNano()%100000)
	script := `source /dev/null 2>/dev/null || true && eval 'echo ` + marker +
		`' && pwd -P >| /dev/null`
	out, errOut, code := inPod(t, `$SHELL -c "`+script+`"`)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	waitForLog(t, "e2e", marker, 3*time.Second)
	log, _, _ := vp(t, "log", "e2e")
	line := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, marker) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the wrapped command was not recorded:\n%s", log)
	}
	if strings.Contains(line, "source ") || strings.Contains(line, "pwd -P") {
		t.Errorf("the log line is the wrapper rather than the command:\n%s", line)
	}
	if !strings.Contains(line, "echo "+marker) {
		t.Errorf("the log line lost the command itself:\n%s", line)
	}
	// The record itself keeps everything: the trust surface is not allowed to
	// abbreviate.
	raw, _, _ := vp(t, "log", "--json", "e2e")
	if !strings.Contains(raw, "pwd -P") {
		t.Errorf("--json dropped part of the command line that actually ran")
	}
}

// --- dispatch --------------------------------------------------------------

// The premise of v2: one command, one named machine, and it is obvious in the
// reading which machine that is.
func TestDispatchRunsOnTheNamedMachine(t *testing.T) {
	out, errOut, code := inRemotePod(t,
		"vp @vptest /usr/bin/env | /usr/bin/grep -c SSH_CONNECTION")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "1" {
		t.Errorf("the command did not run over ssh: %q", out)
	}
	// ...and the converse: without saying so, it stays here. Nothing is inferred
	// from the directory any more, which is the change.
	out, _, _ = inRemotePod(t, "cd "+remoteSrv+
		" && /usr/bin/env | /usr/bin/grep -c SSH_CONNECTION || true")
	if strings.TrimSpace(out) != "0" {
		t.Errorf("a command in a remote directory was shipped without being told: %q", out)
	}
}

func TestDispatchProxiesTheExitCode(t *testing.T) {
	requireSSH(t)
	for _, want := range []int{0, 3, 42} {
		_, errOut, got := inRemotePod(t,
			fmt.Sprintf("vp @vptest /usr/bin/env sh -c 'exit %d'", want))
		if got != want {
			t.Errorf("remote exit %d came back as %d (%s)", want, got, errOut)
		}
	}
}

// A path is the same string on both machines, so a dispatched command needs no
// translation — which is the reason path identity is worth its long prefixes.
func TestADispatchedCommandUsesTheSamePath(t *testing.T) {
	out, errOut, code := inRemotePod(t, "cd "+remoteSrv+" && vp @vptest /usr/bin/pwd")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.TrimSpace(out) != remoteSrv {
		t.Errorf("the working directory did not survive dispatch: %q", out)
	}
}

func TestDispatchNamesTheMachinesItKnowsWhenYouTypoOne(t *testing.T) {
	_, errOut, code := inRemotePod(t, "vp @vptesst /usr/bin/true")
	if code == 0 {
		t.Fatal("a machine that does not exist was accepted")
	}
	if !strings.Contains(errOut, "vptesst") || !strings.Contains(errOut, "vp hosts") {
		t.Errorf("the refusal does not say what to do about it: %q", errOut)
	}
}

// The pod's own machinery never leaves this machine. A command in /vp is the
// socket, a wrapper or the brief, and shipping any of those would be a bug
// dressed as a routing decision.
func TestThePodsOwnDirectoryIsNeverShipped(t *testing.T) {
	_, errOut, code := inRemotePod(t, "cd /vp/bin && vp @vptest /usr/bin/pwd")
	if code == 0 {
		t.Fatal("a command in /vp was dispatched to another machine")
	}
	if !strings.Contains(errOut, "/vp") {
		t.Errorf("the refusal does not name the directory: %q", errOut)
	}
}

// A dispatched command must carry what the caller set for it, and nothing else.
//
// Dropping it entirely was the bug, and it failed silently: a discarded
// PYTHONPATH arrives as ModuleNotFoundError, which reads as a broken install
// rather than a discarded environment. Forwarding everything is not the fix — a
// pod's environment holds the credentials this design exists to keep on one
// machine, and describes this machine rather than the other one.
func TestADispatchedCommandCarriesOnlyWhatTheCallerSet(t *testing.T) {
	requireSSH(t)
	out, errOut, code := inRemotePod(t,
		"VP_PROBE=hello vp @vptest /usr/bin/env | /usr/bin/grep '^VP_PROBE='")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "VP_PROBE=hello") {
		t.Errorf("the caller's variable did not reach the other machine: %q", out)
	}
	out, _, _ = inRemotePod(t,
		"export VP_EXPORTED=yes; vp @vptest /usr/bin/env | /usr/bin/grep '^VP_EXPORTED='")
	if !strings.Contains(out, "VP_EXPORTED=yes") {
		t.Errorf("an exported variable did not reach the other machine: %q", out)
	}
	// The remote's own identity is not overwritten with ours.
	out, _, _ = inRemotePod(t, "vp @vptest /usr/bin/env | /usr/bin/grep '^PATH='")
	if strings.Contains(out, "/vp/bin") {
		t.Errorf("the pod's PATH was imposed on the other machine: %q", out)
	}
	// And the headline promise, with a credential-shaped name.
	out, _, _ = inRemotePod(t,
		"vp @vptest /usr/bin/env | /usr/bin/grep -c VP_FAKE_TOKEN || true")
	if strings.TrimSpace(out) != "0" {
		t.Errorf("a variable from the session baseline crossed: %q", out)
	}
}

// Refusing silently is how the original bug survived, so a variable we decline to
// forward has to be visible somewhere.
func TestARefusedVariableIsReported(t *testing.T) {
	requireSSH(t)
	// An absolute path to `vp`, since the point of the test is a PATH that
	// cannot find anything.
	inRemotePod(t, "PATH=/nonsense /vp/bin/vp @vptest /usr/bin/true")
	time.Sleep(600 * time.Millisecond)
	out, _, _ := vpIn(t, remoteDir, "log", "e2e-remote")
	if !strings.Contains(out, "not forwarded: PATH") {
		t.Errorf("a refused variable was dropped silently:\n%s", out)
	}
}

// --- backends --------------------------------------------------------------

// `vp use` moves the session it is run from, and `vp backend` answers in a word.
// v1 gated this behind a terminal check, reasoning that an agent must not
// re-route itself; in v2 an agent choosing its own machine is the interface.
func TestUseMovesTheSessionAndBackendReportsIt(t *testing.T) {
	out, errOut, code := inRemotePod(t, "vp backend; vp use vptest >/dev/null; vp backend")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Fields(out)
	if len(lines) != 2 || lines[0] != "pod" || lines[1] != "vptest" {
		t.Errorf("backend did not move: %q", out)
	}
}

// Backend is per-session state, never global: the human at a terminal and the
// agent in the pod are separate sessions, so neither can move the other's ground.
func TestBackendIsPerSession(t *testing.T) {
	requireSSH(t)
	if _, _, code := inRemotePod(t, "vp use vptest >/dev/null"); code != 0 {
		t.Fatal("use failed")
	}
	// A different `vp run` is a different session, and must be unaffected.
	out, _, _ := inRemotePod(t, "vp backend")
	if strings.TrimSpace(out) != "pod" {
		t.Errorf("one session's backend leaked into another: %q", out)
	}
}

// A session that has moved still runs its *shell* in the pod, because the
// wrapper an agent generates is bound to this machine. That is a deliberate
// limit, so it must be said out loud rather than discovered.
func TestAMovedSessionIsToldItsShellStaysHere(t *testing.T) {
	requireSSH(t)
	marker := "stay-local-" + fmt.Sprint(time.Now().UnixNano()%100000)
	out, _, _ := inRemotePod(t, "vp use vptest >/dev/null; $SHELL -c 'echo "+
		marker+"; /usr/bin/env | /usr/bin/grep -c SSH_CONNECTION || true'")
	if !strings.Contains(out, marker) {
		t.Fatalf("the command did not run: %q", out)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("a shell was shipped to another machine: %q", out)
	}
	waitForLog(t, "e2e-remote", "shell runs in the pod", 3*time.Second)
	log, _, _ := vpIn(t, remoteDir, "log", "e2e-remote")
	if !strings.Contains(log, "shell runs in the pod") {
		t.Errorf("the limit was not reported anywhere:\n%s", log)
	}
}

// --- tools that belong on another machine ----------------------------------

// A tool that exists only on another machine works by name. The wrapper in
// /vp/bin leads PATH, and it is three lines rather than a bind-mounted shim over
// a binary that had to exist here first.
func TestARemoteOnlyToolIsDispatchedByName(t *testing.T) {
	out, _, _ := inRemotePod(t, "/usr/bin/ls -1 /vp/bin")
	if !strings.Contains(out, "vp-only-tool") {
		t.Fatalf("no wrapper was placed for the remote-only tool:\n%s", out)
	}
	out, errOut, code := inRemotePod(t, "vp-only-tool hello")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "only-on-the-remote hello") {
		t.Errorf("the tool did not run on the machine that has it: %q", out)
	}
	// And the log says where it went, which is the whole point of recording it.
	waitForLog(t, "e2e-remote", "vp-only-tool", 3*time.Second)
	log, _, _ := vpIn(t, remoteDir, "log", "e2e-remote")
	if !strings.Contains(log, "vptest") {
		t.Errorf("the log does not show which machine ran the tool:\n%s", log)
	}
}

// The session's backend wins over the config, because a session that has moved
// to a machine means it.
func TestAToolFollowsTheSessionsBackend(t *testing.T) {
	requireSSH(t)
	out, _, _ := inRemotePod(t, "vp use vptest2 >/dev/null; vp-only-tool second")
	if !strings.Contains(out, "only-on-the-remote second") {
		t.Errorf("the tool did not follow the session to vptest2: %q", out)
	}
}

// waitForLog gives the exit poller time to notice, since vibepod observes
// pod-local commands rather than parenting them.
func waitForLog(t *testing.T, pod, want string, d time.Duration) {
	t.Helper()
	dir := projectDir
	if pod != "e2e" {
		dir = remoteDir
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if out, _, _ := vpIn(t, dir, "log", pod); strings.Contains(out, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var _ = filepath.Join
