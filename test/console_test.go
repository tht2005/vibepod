package e2e

import (
	"strings"
	"testing"
	"time"
)

// The console is the first thing a new user sees, so it is worth a test that
// actually types into it.
func TestConsoleRunsCommandsAndNamesTheirRoutes(t *testing.T) {
	con := onPTY(t, "new", "e2e")
	defer con.stop()

	if !con.waitFor(t, "vibepod · e2e", 10*time.Second) {
		t.Fatalf("no console header; got:\n%s", con.out.String())
	}
	if !con.waitFor(t, "❯", 10*time.Second) {
		t.Fatalf("no prompt; got:\n%s", con.out.String())
	}

	con.send("/usr/bin/ls /\r")
	// A command's own output must survive. The live pane redraws by erasing
	// the current line, and the current line belongs to whatever is running.
	if !con.waitFor(t, "usr", 15*time.Second) {
		t.Fatalf("the command's output never appeared; got:\n%s", con.out.String())
	}
	// The live pane is the reason the console exists: every command shows the
	// machine it ran on.
	if !con.waitFor(t, "pod", 10*time.Second) {
		t.Errorf("the exec log did not name a target; got:\n%s", con.out.String())
	}
	// ...once. The console wraps each line in a shell, and a shell is never
	// routed, so logging it says the same thing twice with the wrong answer.
	if n := strings.Count(con.out.String(), "/bin/sh -c"); n > 0 {
		t.Errorf("the console logged its own shell wrapper %d time(s):\n%s",
			n, con.out.String())
	}

	// The console keeps the keyboard after a command finishes. It did not,
	// once: the daemon was reading this terminal, and a read blocked on a tty
	// does not come back when the descriptor is closed, so it went on eating
	// every keystroke that followed.
	con.send("/usr/bin/echo and-again\r")
	if !con.waitFor(t, "and-again", 15*time.Second) {
		t.Fatalf("the console lost the keyboard after one command; got:\n%s",
			con.out.String())
	}

	// Leaving the console must not take the pod with it: the whole point is
	// that work outlives the terminal watching it.
	con.send("exit\r")
	if !con.waitFor(t, "is still running", 10*time.Second) {
		t.Errorf("exit did not say the pod survives; got:\n%s", con.out.String())
	}
}

// cd changes where commands run, so the console has to validate it against the
// pod's filesystem rather than the host's.
func TestConsoleRefusesADirectoryThePodCannotSee(t *testing.T) {
	con := onPTY(t, "new", "e2e")
	defer con.stop()
	if !con.waitFor(t, "❯", 10*time.Second) {
		t.Fatalf("no prompt; got:\n%s", con.out.String())
	}
	con.send("cd /definitely-not-here\r")
	if !con.waitFor(t, "no such directory in the pod", 10*time.Second) {
		t.Errorf("cd was accepted for a path the pod cannot see; got:\n%s",
			con.out.String())
	}
	if strings.Contains(con.out.String(), "definitely-not-here ❯") {
		t.Errorf("the prompt moved to a directory that does not exist")
	}
}
