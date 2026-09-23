package e2e

import (
	"strings"
	"testing"
	"time"
)

// M5: the cockpit.
//
// It is a cockpit and not a multiplexer. ⏎ does not draw a terminal inside a
// pane: it leaves the alt-screen, hands the raw terminal to the session, and
// redraws when you come back. These tests drive it through a real pty, because
// every interesting property of a terminal program is a property of the bytes it
// writes to one.

func cockpit(t *testing.T, dir, pod string) *ptyRun {
	t.Helper()
	r := onPTYEnv(t, dir, tuiEnv, "cockpit", pod)
	if !r.seeScreen(t, "MACHINES", 20*time.Second) {
		t.Fatalf("the cockpit never drew a frame; got:\n%s", r.screen())
	}
	return r
}

func TestCockpitShowsTheMachinesTheSessionsAndTheLog(t *testing.T) {
	dir := livePod(t, "e2e-tui")
	r := cockpit(t, dir, "e2e-tui")
	defer r.stop()
	// The cockpit draws as soon as it has anything and fills in as answers
	// arrive, so each of these is waited for rather than expected in frame one.
	for _, want := range []string{"MACHINES", "SESSIONS", "ACTIVITY", "MOUNTS",
		"vptest", "backend", "⏎ attach", "q quit"} {
		if !r.seeScreen(t, want, 10*time.Second) {
			t.Errorf("the cockpit does not show %q:\n%s", want, r.screen())
		}
	}
	// A machine it could mount but has not is listed with the command that would
	// mount it: "what else is there" is the question `m` answers.
	if !r.seeScreen(t, "vp mount vptest2", 10*time.Second) {
		t.Errorf("the cockpit does not offer the unmounted machine:\n%s",
			r.screen())
	}
	r.send("q")
}

// `m` is the whole of M4 behind one key.
func TestCockpitMountsWithOneKey(t *testing.T) {
	dir := livePod(t, "e2e-tui-mount")
	r := cockpit(t, dir, "e2e-tui-mount")
	defer r.stop()
	r.send("m")
	if !r.seeScreen(t, "mount (host:/path)", 5*time.Second) {
		t.Fatalf("`m` did not prompt; got:\n%s", r.screen())
	}
	r.send("vptest2:" + remoteAlt + "\r")
	if !r.seeScreen(t, "done", 25*time.Second) {
		t.Fatalf("the mount did not report back; got:\n%s", r.screen())
	}
	out, _, _ := vpIn(t, dir, "tree", "--json", "e2e-tui-mount")
	if !strings.Contains(out, remoteAlt) {
		t.Errorf("the mount did not happen:\n%s", out)
	}
	r.send("q")
}

// `b` moves the focused session, which is where a human chooses the machine.
func TestCockpitMovesASessionsBackend(t *testing.T) {
	dir := livePod(t, "e2e-tui-backend")
	sh := onPTYIn(t, dir, "shell", "--raw", "e2e-tui-backend")
	defer sh.stop()
	sh.ready(t, 15*time.Second)
	id := sh.sessionID(t)

	r := cockpit(t, dir, "e2e-tui-backend")
	defer r.stop()
	if !r.seeScreen(t, "▸ pod", 10*time.Second) {
		t.Fatalf("the cockpit does not show the session's machine; got:\n%s",
			r.screen())
	}
	r.send("\t") // focus the sessions
	r.send("b")
	if !r.seeScreen(t, "backend for this session", 5*time.Second) {
		t.Fatalf("`b` did not prompt; got:\n%s", r.screen())
	}
	r.send("vptest\r")
	if !r.seeScreen(t, "now on vptest", 20*time.Second) {
		t.Fatalf("`b` did not move the session; got:\n%s", r.screen())
	}
	// And the terminal that owns the session hears about it, since the shell its
	// keystrokes reach has changed.
	if !sh.waitFor(t, "now on vptest", 20*time.Second) {
		t.Errorf("the session's own terminal was not told; got:\n%s", sh.out.String())
	}
	out, _, _ := vpIn(t, dir, "ps")
	if !strings.Contains(out, "vptest") {
		t.Errorf("ps does not show the moved session:\n%s", out)
	}
	_ = id
	r.send("q")
}

// Handoff, not nesting: the cockpit leaves the alt-screen, the session gets the
// raw terminal, and the cockpit redraws when the session is detached from.
//
// The detach key belongs to the session rather than to the cockpit — swallowing
// keystrokes that an agent wants is exactly the failure this design avoids.
func TestCockpitHandsTheTerminalToASessionAndTakesItBack(t *testing.T) {
	dir := livePod(t, "e2e-tui-handoff")
	// A session to attach to, left running by killing its client rather than
	// exiting it.
	sh := onPTYIn(t, dir, "shell", "--raw", "e2e-tui-handoff")
	sh.ready(t, 15*time.Second)
	sh.send("VP_MARK=handoff-probe\n")
	sh.send("echo se''t:$VP_MARK\n")
	if !sh.waitFor(t, "set:handoff-probe", 10*time.Second) {
		t.Fatalf("could not set up the session; got:\n%s", sh.out.String())
	}
	// Killing the client, not exiting the shell: a terminal that disappears is a
	// detach, and the session has to be there to attach to.
	sh.stop()

	r := cockpit(t, dir, "e2e-tui-handoff")
	defer r.stop()
	if !r.seeScreen(t, "▸ pod", 10*time.Second) {
		t.Fatalf("the cockpit does not list the session; got:\n%s", r.screen())
	}
	r.send("\t")
	mark := len(r.screen())
	r.send("\r")
	// The alt-screen is left on the way out, and the session's own shell answers.
	if !r.waitFor(t, "\x1b[?1049l", 10*time.Second) {
		t.Fatalf("the cockpit did not leave the alt-screen; got:\n%s",
			r.out.String()[mark:])
	}
	r.send("echo mar''k:$VP_MARK\n")
	if !r.waitFor(t, "mark:handoff-probe", 15*time.Second) {
		t.Fatalf("the session did not get the terminal; got:\n%s", r.out.String()[mark:])
	}
	// Ctrl-\ is the session's detach, and the cockpit comes back.
	r.send("\x1c")
	if !r.seeScreen(t, "left running", 15*time.Second) {
		t.Fatalf("the cockpit did not take the terminal back; got:\n%s", r.screen())
	}
	if !r.altScreen() {
		t.Errorf("the cockpit did not re-enter the alt-screen after the handoff")
	}
	r.send("q")
}

func TestCockpitQuitsWithoutStoppingThePod(t *testing.T) {
	dir := livePod(t, "e2e-tui-quit")
	r := cockpit(t, dir, "e2e-tui-quit")
	defer r.stop()
	r.send("q")
	if !r.waitFor(t, "\x1b[?1049l", 10*time.Second) {
		t.Errorf("quitting did not restore the screen; got:\n%s", r.screen())
	}
	// The pod is the point: closing the cockpit is not closing the work.
	out, _, code := vpIn(t, dir, "ps")
	if code != 0 || !strings.Contains(out, "e2e-tui-quit") {
		t.Errorf("the pod went down with the cockpit:\n%s", out)
	}
}

// A session `vp shell` started comes back from the cockpit as blocks, because
// ⏎ is `vp attach`, and `vp attach` knows which interface a session speaks.
func TestCockpitOpensABlockSessionAsBlocks(t *testing.T) {
	dir := livePod(t, "e2e-tui-blocks-handoff")
	sh := tui(t, dir, "e2e-tui-blocks-handoff")
	sh.run(t, "VP_MARK=from-blocks; echo se''t", "set")
	sh.send("\x1c")
	if !sh.seeScreen(t, "detached", 10*time.Second) {
		t.Fatalf("could not detach:\n%s", sh.screen())
	}
	sh.stop()

	r := cockpit(t, dir, "e2e-tui-blocks-handoff")
	defer r.stop()
	if !r.seeScreen(t, "▸ pod", 10*time.Second) {
		t.Fatalf("the cockpit does not list the session:\n%s", r.screen())
	}
	r.send("\t\r")
	if !r.seeScreen(t, "run on pod", 15*time.Second) {
		t.Fatalf("⏎ did not open the block interface:\n%s", r.screen())
	}
	r.run(t, "echo mark:$VP_MARK", "mark:from-blocks")
	r.send("\x1c")
	if !r.seeScreen(t, "left running", 15*time.Second) {
		t.Fatalf("the cockpit did not come back:\n%s", r.screen())
	}
	r.send("q")
}
