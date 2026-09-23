package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vibepod/internal/sys"
)

// onPTY runs vpctl attached to a terminal of our own making, which is the
// only way to exercise the parts of vibepod that exist because terminals do.
type ptyRun struct {
	master *os.File
	cmd    *exec.Cmd
	out    bytes.Buffer
}

func onPTY(t *testing.T, args ...string) *ptyRun {
	t.Helper()
	master, slave, err := sys.OpenPTY()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	cmd := exec.Command(binDir+"/vpctl", args...)
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "VIBEPOD_RUNDIR="+runDir, "SHELL=/bin/sh",
		"TERM=dumb", "PS1=$ ")
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
