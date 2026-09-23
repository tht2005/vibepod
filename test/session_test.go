package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vibepod/internal/sys"
)

// sessionID asks the shell which session it is, which is also a check that the
// daemon exports it. Reading `vp ps` instead would find whichever session
// happened to be on that machine, and a pod has several.
func (r *ptyRun) sessionID(t *testing.T) string {
	t.Helper()
	r.send("echo se''ss:$VIBEPOD_SESSION\n")
	if !r.waitFor(t, "sess:", 5*time.Second) {
		t.Fatalf("the session did not report its id; got:\n%s", r.out.String())
	}
	for _, line := range strings.Split(r.out.String(), "\n") {
		if i := strings.Index(line, "sess:"); i >= 0 {
			id := strings.TrimSpace(line[i+len("sess:"):])
			if id = strings.Fields(id + " ")[0]; id != "" && id != "$VIBEPOD_SESSION" {
				return id
			}
		}
	}
	t.Fatalf("could not read the session id; got:\n%s", r.out.String())
	return ""
}

// onPTY runs the client attached to a terminal of our own making, which is the
// only way to exercise the parts of vibepod that exist because terminals do.
type ptyRun struct {
	master *os.File
	cmd    *exec.Cmd
	out    bytes.Buffer
}

func onPTY(t *testing.T, args ...string) *ptyRun {
	t.Helper()
	return onPTYIn(t, projectDir, args...)
}

func onPTYIn(t *testing.T, dir string, args ...string) *ptyRun {
	t.Helper()
	master, slave, err := sys.OpenPTY()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	// A terminal with no size is not a terminal anything can draw on, and a
	// fresh pty has none until somebody says otherwise.
	if err := sys.SetWinsize(master.Fd(), 24, 100); err != nil {
		t.Fatalf("set winsize: %v", err)
	}
	cmd := exec.Command(binDir+"/vp", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir, "SHELL=/bin/sh",
		"TERM=dumb", "PS1=$ ")
	if ssh != nil {
		cmd.Env = append(cmd.Env, "VIBEPOD_SSH_CONFIG="+ssh.configFile)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscallSysProcAttr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	slave.Close()
	r := &ptyRun{master: master, cmd: cmd}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				r.out.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

func (r *ptyRun) send(s string) { _, _ = r.master.WriteString(s) }

// ready waits until a shell is answering, without assuming what its prompt looks
// like. A shell on another machine is that machine's own login shell, with
// whatever prompt its owner configured — so asking it to say something is the
// only portable readiness check.
func (r *ptyRun) ready(t *testing.T, d time.Duration) {
	t.Helper()
	n := time.Now().UnixNano() % 1000000
	// The local pty echoes what we type, so a token that appears verbatim in the
	// input would match its own echo and prove nothing. Split it with quotes: the
	// shell joins them, the echo does not.
	typed := fmt.Sprintf("echo rea''dy-%d\n", n)
	want := fmt.Sprintf("ready-%d", n)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r.send(typed)
		if r.waitFor(t, want, 2*time.Second) {
			return
		}
	}
	t.Fatalf("no shell answered in %s; got:\n%s", d, r.out.String())
}

// waitFor gives the session a bounded amount of time to say something.
func (r *ptyRun) waitFor(t *testing.T, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(r.out.String(), want) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func (r *ptyRun) stop() {
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_, _ = r.cmd.Process.Wait()
	}
	r.master.Close()
}

// A session must survive the terminal that started it. This is the whole
// point of the daemon owning the pty rather than the client.
func TestDetachLeavesTheSessionRunningAndAttachReplaysIt(t *testing.T) {
	marker := "marker-4f21c"
	first := onPTY(t, "shell", "e2e")
	defer first.stop()

	if !first.waitFor(t, "$", 10*time.Second) {
		t.Fatalf("no shell prompt; got:\n%s", first.out.String())
	}
	first.send("echo " + marker + "\n")
	if !first.waitFor(t, marker, 10*time.Second) {
		t.Fatalf("command produced nothing; got:\n%s", first.out.String())
	}
	first.send("\x1c") // Ctrl-\ : detach, do not kill
	if !first.waitFor(t, "detached", 10*time.Second) {
		t.Fatalf("did not detach; got:\n%s", first.out.String())
	}

	second := onPTY(t, "attach", "e2e")
	defer second.stop()
	if !second.waitFor(t, marker, 10*time.Second) {
		t.Errorf("reattaching did not replay the scrollback; got:\n%s",
			second.out.String())
	}
	// The session is still live, not merely remembered.
	second.send("echo still-" + marker + "\n")
	if !second.waitFor(t, "still-"+marker, 10*time.Second) {
		t.Errorf("session did not survive the detach; got:\n%s", second.out.String())
	}
	second.send("exit\n")
}

// A pod is not one terminal. Each session has its own working directory and its
// own backend, which is what makes `vp shell` useful next to a running agent
// rather than instead of it.
func TestSeveralSessionsOnOnePod(t *testing.T) {
	first := onPTY(t, "shell", "e2e")
	defer first.stop()
	second := onPTY(t, "shell", "e2e")
	defer second.stop()

	for i, r := range []*ptyRun{first, second} {
		if !r.waitFor(t, "$", 10*time.Second) {
			t.Fatalf("session %d never got a prompt; got:\n%s", i+1, r.out.String())
		}
	}
	// Distinct shells: a directory change in one must not move the other.
	first.send("cd /tmp && echo first-is:$PWD\n")
	if !first.waitFor(t, "first-is:/tmp", 10*time.Second) {
		t.Fatalf("session 1 did not run; got:\n%s", first.out.String())
	}
	second.send("echo second-is:$PWD\n")
	if !second.waitFor(t, "second-is:", 10*time.Second) {
		t.Fatalf("session 2 did not run; got:\n%s", second.out.String())
	}
	if strings.Contains(second.out.String(), "second-is:/tmp\r") {
		t.Errorf("the two sessions share a working directory")
	}

	out, _, code := vp(t, "ps")
	if code != 0 {
		t.Fatalf("ps exit %d", code)
	}
	// ps counts sessions; both shells should be in it.
	if !strings.Contains(out, "e2e") {
		t.Fatalf("ps lost the pod:\n%s", out)
	}
	tree, _, _ := vp(t, "tree", "e2e")
	if n := strings.Count(tree, "session "); n < 2 {
		t.Errorf("tree shows %d sessions, want at least 2:\n%s", n, tree)
	}
	first.send("exit\n")
	second.send("exit\n")
}

// The M3 success criterion: an attached session whose backend is another machine
// *is* a shell on that machine, so `cd` persists.
//
// Nothing here is emulated, and that is the point. v1 opened one ssh per command,
// which is why `cd` did not stick, `export` did not stick and job control did not
// exist; DESIGN.md §3 calls that the class of problem a session-bound shell fixes
// by not reproducing anything.
func TestAnAttachedSessionOnAnotherMachineIsThatMachinesShell(t *testing.T) {
	requireSSH(t)
	r := onPTYIn(t, remoteDir, "shell", "-on", "vptest", "e2e-remote")
	defer r.stop()
	r.ready(t, 20*time.Second)
	// It really is over there.
	r.send("echo conn:${SSH_CONNECTION:+yes}\n")
	if !r.waitFor(t, "conn:yes", 10*time.Second) {
		t.Fatalf("the session is not a shell on the other machine; got:\n%s",
			r.out.String())
	}
	// cd persists, because it is cd.
	r.send("cd " + remoteSrv + "\n")
	r.send("pwd\n")
	if !r.waitFor(t, remoteSrv, 10*time.Second) {
		t.Errorf("cd did not persist in the session; got:\n%s", r.out.String())
	}
	// So does a shell variable, which per-command ssh could never do.
	r.send("VP_STICKY=yes\n")
	r.send("echo sticky:$VP_STICKY\n")
	if !r.waitFor(t, "sticky:yes", 10*time.Second) {
		t.Errorf("a shell variable did not persist; got:\n%s", r.out.String())
	}
	// And the pod knows which machine that session is on.
	out, _, _ := vpIn(t, remoteDir, "tree", "e2e-remote")
	if !strings.Contains(out, "vptest") {
		t.Errorf("the tree does not show the session's machine:\n%s", out)
	}
	r.send("exit\n")
}

// Moving an attached session opens a shell on the new machine and leaves the old
// one alone: switching back finds it with its cwd and its variables intact.
func TestMovingAnAttachedSessionLeavesTheOtherShellAlone(t *testing.T) {
	requireSSH(t)
	r := onPTYIn(t, remoteDir, "shell", "e2e-remote")
	defer r.stop()
	r.ready(t, 15*time.Second)
	id := r.sessionID(t)
	r.send("VP_HERE=pod-side\n")
	r.send("vp use vptest\n")
	if !r.waitFor(t, "now on vptest", 15*time.Second) {
		t.Fatalf("the terminal was not moved; got:\n%s", r.out.String())
	}
	r.send("echo there:${SSH_CONNECTION:+yes}\n")
	if !r.waitFor(t, "there:yes", 10*time.Second) {
		t.Fatalf("the terminal did not reach the other machine; got:\n%s",
			r.out.String())
	}
	// From over there `vp` does not exist, and must not: nothing is installed on
	// another machine. The way back is the machine that owns the session.
	if out, errOut, code := vpIn(t, remoteDir, "use", "-s", id, "pod"); code != 0 {
		t.Fatalf("could not move the session back: %s %s", out, errOut)
	}
	if !r.waitFor(t, "now on pod", 15*time.Second) {
		t.Fatalf("the terminal did not come back; got:\n%s", r.out.String())
	}
	r.send("echo kept:$VP_HERE\n")
	if !r.waitFor(t, "kept:pod-side", 10*time.Second) {
		t.Errorf("the shell left behind did not keep its state; got:\n%s",
			r.out.String())
	}
	r.send("exit\n")
}
