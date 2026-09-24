package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// `vp shell`: the block interface.
//
// What matters is what a person sees, so these tests put the pty's bytes
// through a terminal emulator and read the screen and its scrollback, the way a
// terminal would show them — not the byte stream, where a word can be split by
// the escape sequences that colour it.

// screen is what a terminal of the test pty's size would show, scrollback
// first.
func (r *ptyRun) screen() string {
	e := vt.NewEmulator(100, 24)
	e.SetScrollbackSize(100000)
	go func() {
		// The emulator answers queries on its input pipe, which must be drained.
		b := make([]byte, 1024)
		for {
			if _, err := e.Read(b); err != nil {
				return
			}
		}
	}()
	defer e.Close()
	_, _ = e.Write([]byte(r.out.String()))
	var b strings.Builder
	for _, l := range e.Scrollback().Lines() {
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")
	}
	b.WriteString(e.String())
	return b.String()
}

// altScreen is whether a terminal fed this output would be on its alternate
// screen now.
func (r *ptyRun) altScreen() bool {
	e := vt.NewEmulator(100, 24)
	go func() {
		b := make([]byte, 1024)
		for {
			if _, err := e.Read(b); err != nil {
				return
			}
		}
	}()
	defer e.Close()
	_, _ = e.Write([]byte(r.out.String()))
	return e.IsAltScreen()
}

func (r *ptyRun) seeScreen(t *testing.T, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(r.screen(), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// tui opens `vp shell` on a pty and waits for its input line.
func tui(t *testing.T, dir string, args ...string) *ptyRun {
	t.Helper()
	r := onPTYEnv(t, dir, tuiEnv, append([]string{"shell"}, args...)...)
	if !r.seeScreen(t, "run on", 30*time.Second) {
		r.stop()
		t.Fatalf("vp shell never reached its input line; screen:\n%s", r.screen())
	}
	return r
}

// tuiEnv is a terminal that can draw, and a shell the hooks know.
var tuiEnv = []string{"TERM=xterm-256color", "SHELL=/bin/bash"}

// run types a line and waits for something the command prints.
func (r *ptyRun) run(t *testing.T, line, want string) {
	t.Helper()
	r.send(line + "\r")
	if !r.seeScreen(t, want, 15*time.Second) {
		t.Fatalf("after %q, the screen never showed %q:\n%s", line, want, r.screen())
	}
}

func TestShellShowsEachCommandAsABlock(t *testing.T) {
	dir := livePod(t, "e2e-tui-blocks")
	r := tui(t, dir, "e2e-tui-blocks")
	defer r.stop()

	// The header is the command as typed, the output follows, and a failure
	// says so with its code.
	r.run(t, "echo hel''lo-block; false", "✗ exit 1")
	s := r.screen()
	for _, want := range []string{"$ echo hel''lo-block; false", "hello-block"} {
		if !strings.Contains(s, want) {
			t.Errorf("the block lacks %q:\n%s", want, s)
		}
	}
	// None of the shell's own prompt, and none of the hooks, reach the screen.
	if strings.Contains(s, "__vp_") || strings.Contains(s, "133;") {
		t.Errorf("the hooks leaked onto the screen:\n%s", s)
	}

	// It is one shell: cd and variables carry from one command to the next,
	// and the status line follows the directory.
	r.run(t, "cd /tmp && VP_TUI=kept && echo cd-d''one", "cd-done")
	r.run(t, "echo var:$VP_TUI pwd:$(pwd)", "var:kept pwd:/tmp")
}

// Between the markers is a real terminal: a program that asks a question gets
// the keys, and one that takes the whole screen gets the terminal.
func TestShellGivesTheRunningProgramTheKeyboard(t *testing.T) {
	dir := livePod(t, "e2e-tui-keys")
	r := tui(t, dir, "e2e-tui-keys")
	defer r.stop()

	r.run(t, `printf 'name? '; read x; echo got-$x`, "name?")
	r.send("bob\r")
	if !r.seeScreen(t, "got-bob", 10*time.Second) {
		t.Fatalf("the program did not get the keys:\n%s", r.screen())
	}
	// ctrl+c reaches the program as the interrupt it is.
	r.run(t, "sleep 30; echo not-interrupted", "$ sleep 30")
	time.Sleep(300 * time.Millisecond)
	r.send("\x03")
	if !r.seeScreen(t, "✗ exit 130", 10*time.Second) {
		t.Fatalf("ctrl+c did not interrupt the command:\n%s", r.screen())
	}

	// A full-screen program: the alternate screen, keys, and the blocks back.
	// FULL-%s: the header shows the command line, and the test must wait for the
	// program, not for its own name.
	r.run(t, `printf '\033[?1049hFULL-%s' SCREEN; read y; printf '\033[?1049l'; echo out-$y`,
		"FULL-SCREEN")
	r.send("zz\r")
	if !r.seeScreen(t, "out-zz", 10*time.Second) {
		t.Fatalf("the full-screen program did not hand the terminal back:\n%s", r.screen())
	}
	if !r.seeScreen(t, "run on", 10*time.Second) {
		t.Fatalf("the input line did not come back:\n%s", r.screen())
	}
}

// Detaching leaves the shell exactly where it was, and attaching again brings
// back the interface — including a command that was still running.
func TestShellDetachesAndComesBack(t *testing.T) {
	dir := livePod(t, "e2e-tui-detach")
	r := tui(t, dir, "e2e-tui-detach")
	r.run(t, "VP_LEFT=still-here; echo id:$VIBEPOD_SESSION", "id:")
	id := ""
	for _, l := range strings.Split(r.screen(), "\n") {
		if i := strings.Index(l, "id:"); i >= 0 && !strings.Contains(l, "$") {
			id = strings.TrimSpace(l[i+3:])
		}
	}
	if id == "" {
		t.Fatalf("no session id on screen:\n%s", r.screen())
	}
	r.run(t, "sleep 4; echo finished-while-a''way", "$ sleep 4")
	r.send("\x1c")
	if !r.seeScreen(t, "detached", 10*time.Second) {
		t.Fatalf("ctrl+\\ did not detach:\n%s", r.screen())
	}
	r.stop()

	a := onPTYEnv(t, dir, tuiEnv, "attach", "e2e-tui-detach", id)
	defer a.stop()
	if !a.seeScreen(t, "run on", 20*time.Second) {
		t.Fatalf("attach did not bring the interface back:\n%s", a.screen())
	}
	// The command that was running when we left is shown as the block it is,
	// and finishes in front of us.
	if !a.seeScreen(t, "(was running)", 5*time.Second) {
		t.Errorf("the running command was not picked up again:\n%s", a.screen())
	}
	if !a.seeScreen(t, "finished-while-away", 10*time.Second) {
		t.Fatalf("the command's output after reattaching never came:\n%s", a.screen())
	}
	if !a.seeScreen(t, "run on", 10*time.Second) {
		t.Fatalf("no input line after the command finished:\n%s", a.screen())
	}
	a.run(t, "echo left:$VP_LEFT", "left:still-here")
}

// `vp use` inside vp shell moves it to another machine's shell, which gets the
// hooks on first sight, and whose blocks are drawn in that machine's colour.
func TestShellMovesToAnotherMachine(t *testing.T) {
	requireSSH(t)
	r := tui(t, remoteDir, "e2e-remote")
	defer r.stop()
	r.run(t, "VP_POD_SIDE=yes; vp use vptest", "now on vptest")
	if !r.seeScreen(t, "run on vptest", 20*time.Second) {
		t.Fatalf("the shell on vptest never reached a prompt:\n%s", r.screen())
	}
	r.run(t, "echo conn:${SSH_CONNECTION:+yes}", "conn:yes")
	// Back to the pod from outside, the way the raw shell does it: the pod's
	// shell is where it was, variables and all.
	id := ""
	out, _, _ := vpIn(t, remoteDir, "ps", "e2e-remote")
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "vptest") && strings.Contains(l, "shell") {
			if f := strings.Fields(l); len(f) > 0 {
				id = f[0]
			}
		}
	}
	if id == "" {
		t.Skipf("vp ps does not list the session in a form this test reads:\n%s", out)
	}
	if _, errOut, code := vpIn(t, remoteDir, "use", "pod", "-s", id); code != 0 {
		t.Fatalf("vp use pod -s %s: %s", id, errOut)
	}
	if !r.seeScreen(t, "run on pod", 15*time.Second) {
		t.Fatalf("did not come back to the pod:\n%s", r.screen())
	}
	r.run(t, "echo side:$VP_POD_SIDE", "side:yes")
}

// A session on another machine is that machine's login shell — here zsh or
// bash, whatever the fixture user has — and its blocks say so.
func TestShellOnAnotherMachine(t *testing.T) {
	requireSSH(t)
	r := tui(t, remoteDir, "-on", "vptest", "e2e-remote")
	defer r.stop()
	r.run(t, "echo conn:${SSH_CONNECTION:+yes}", "conn:yes")
	if !strings.Contains(r.screen(), "vptest") {
		t.Errorf("the block does not say which machine:\n%s", r.screen())
	}
	r.run(t, "cd "+remoteSrv+" && pwd", remoteSrv)
	n := time.Now().UnixNano() % 100000
	r.run(t, fmt.Sprintf("echo still:$(pwd)-%d", n), fmt.Sprintf("still:%s-%d", remoteSrv, n))
	// ctrl+d at an empty line is exit, and the session ends with vp shell.
	r.send("\x04")
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Errorf("ctrl+d did not end vp shell:\n%s", r.screen())
	}
}

// `clear` clears what a person sees, not just the block it ran in: the blocks
// before it are in the terminal's scrollback, so that is what goes. /clear does
// the same with no shell involved.
func TestShellClearClearsTheTerminal(t *testing.T) {
	dir := livePod(t, "e2e-tui-clear")
	r := tui(t, dir, "e2e-tui-clear")
	defer r.stop()

	r.run(t, "echo before-cl''ear", "before-clear")
	r.send("clear\r")
	deadline := time.Now().Add(10 * time.Second)
	for strings.Contains(r.screen(), "before-clear") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(r.screen(), "before-clear") {
		t.Fatalf("`clear` left the earlier blocks:\n%s", r.screen())
	}
	r.run(t, "echo after-cl''ear", "after-clear")

	r.send("/clear\r")
	deadline = time.Now().Add(10 * time.Second)
	for strings.Contains(r.screen(), "after-clear") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(r.screen(), "after-clear") {
		t.Fatalf("/clear left the earlier blocks:\n%s", r.screen())
	}
	if !r.seeScreen(t, "run on", 5*time.Second) {
		t.Fatalf("no input line after /clear:\n%s", r.screen())
	}
}

// /name is the interface's own command only when the name is registered: a path
// still reaches the shell, and a leading space sends even a registered name there.
func TestShellSlashCommandsLeavePathsToTheShell(t *testing.T) {
	dir := livePod(t, "e2e-tui-slash")
	r := tui(t, dir, "e2e-tui-slash")
	defer r.stop()

	r.run(t, "/help", "vp shell commands")
	r.run(t, "/bin/echo slash-path-ran", "slash-path-ran")
	r.run(t, " /help", "$  /help")
	if !r.seeScreen(t, "✗ exit 127", 10*time.Second) {
		t.Fatalf("a leading space did not send /help to the shell:\n%s", r.screen())
	}
	// Typing / opens the menu of commands.
	r.send("/cle")
	if !r.seeScreen(t, "clear the screen and the scrollback", 5*time.Second) {
		t.Fatalf("typing /cle did not offer /clear:\n%s", r.screen())
	}
	r.send("\x15") // ctrl+u: the line goes, and the menu with it
}

// Tab asks the shell on the session's machine, in the directory it is in.
func TestShellTabCompletesWithTheShell(t *testing.T) {
	dir := livePod(t, "e2e-tui-tab")
	r := tui(t, dir, "e2e-tui-tab")
	defer r.stop()

	name := fmt.Sprintf("vp-tab-%d", time.Now().UnixNano()%100000)
	r.run(t, "cd "+workDir+" && mkdir -p "+name+"-only/inside && echo made-it", "made-it")
	r.send("ls " + name + "-o\t")
	if !r.seeScreen(t, "ls "+name+"-only/", 15*time.Second) {
		t.Fatalf("tab did not complete the directory:\n%s", r.screen())
	}
	r.send("\r")
	if !r.seeScreen(t, "inside", 10*time.Second) {
		t.Fatalf("the completed line did not run:\n%s", r.screen())
	}
	r.run(t, "rm -r "+name+"-only && echo cleaned", "cleaned")
}

// visible is the terminal's screen alone, without its scrollback, one string
// per row.
func (r *ptyRun) visible() []string {
	e := vt.NewEmulator(100, 24)
	go func() {
		b := make([]byte, 1024)
		for {
			if _, err := e.Read(b); err != nil {
				return
			}
		}
	}()
	defer e.Close()
	_, _ = e.Write([]byte(r.out.String()))
	return strings.Split(e.String(), "\n")
}

// The input box is on the bottom rows from the first moment, and output fills
// the screen from just above it; what no longer fits goes to the terminal's own
// scrollback, once each and in order.
func TestShellInputStaysAtTheBottom(t *testing.T) {
	dir := livePod(t, "e2e-tui-bottom")
	r := tui(t, dir, "e2e-tui-bottom")
	defer r.stop()

	boxRow := func() int {
		for i, l := range r.visible() {
			if strings.Contains(l, "❯") {
				return i
			}
		}
		return -1
	}
	if got := boxRow(); got != 21 {
		t.Fatalf("the input line is on row %d of 24, not above the status line:\n%s",
			got+1, strings.Join(r.visible(), "\n"))
	}
	r.run(t, "seq 1 60 | sed s/^/n/", "n60")
	if !r.seeScreen(t, "run on", 10*time.Second) {
		t.Fatalf("no input line after the command:\n%s", r.screen())
	}
	if got := boxRow(); got != 21 {
		t.Errorf("the input line moved to row %d:\n%s", got+1, strings.Join(r.visible(), "\n"))
	}
	// Every line once, in order, across the scrollback and the screen.
	var seen []string
	for _, l := range strings.Split(r.screen(), "\n") {
		if f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "▌")); len(f) == 1 &&
			strings.HasPrefix(f[0], "n") {
			seen = append(seen, f[0])
		}
	}
	if len(seen) != 60 || seen[0] != "n1" || seen[59] != "n60" {
		t.Errorf("the output did not reach the scrollback once each, in order: %v", seen)
	}
}
