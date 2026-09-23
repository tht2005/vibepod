package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
	"vibepod/internal/route"
	"vibepod/internal/term"
)

// The console is a cockpit, not a shell replacement.
//
// Competing with zsh means rebuilding completion, history, job control and a
// terminal emulator, and losing anyway. What a shell cannot show you is where
// your commands are going — so that is what this shows, above a prompt that
// hands real work to the pod.
type console struct {
	conn    *proto.Conn // event stream
	spec    *proto.Msg
	pod     string
	session string
	cwd     string
	prev    string // for `cd -`
	pin     string // set by `use`; overrides what the directory would decide
	table   *route.Table

	mu      sync.Mutex
	line    []rune
	state   *term.State
	cmdConn *proto.Conn // non-nil while a command owns the terminal
	held    []string    // log lines that arrived while it did
}

func cmdNew(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	m, err := loadSpec(name, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	defer c.Close()
	if err := ensureUp(c, m); err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	if !term.IsTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "vpctl: the console needs a terminal; "+
			"use `vpctl up` for scripts")
		return 1
	}
	rules := make([]route.Rule, 0, len(m.Routes))
	for _, r := range m.Routes {
		rules = append(rules, route.Rule{Prefix: r.Prefix, Target: r.Target,
			RemotePrefix: r.RemotePrefix})
	}
	con := &console{
		spec: m, pod: m.Spec.Name, cwd: podCwd(m.Spec.Binds),
		session: "console", table: route.New(m.ExecDefault, rules),
	}
	return con.run()
}

func (con *console) run() int {
	state, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	con.state = state
	defer state.Restore()

	con.header()
	go con.followEvents()
	con.prompt()

	// One read loop for the console's whole life. While a command is running
	// the same loop forwards keystrokes to it, which is what "hands the
	// terminal over" means in practice — and why the daemon never needs to
	// read this terminal itself.
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0
		}
		if c := con.commandConn(); c != nil {
			if !sendInput(c, buf[:n]) {
				con.out("\r\n[detached — the command is still running]\r\n")
			}
			continue
		}
		for _, b := range buf[:n] {
			switch b {
			case '\r', '\n':
				line := strings.TrimSpace(string(con.line))
				con.mu.Lock()
				con.line = nil
				con.mu.Unlock()
				con.out("\r\n")
				if quit := con.submit(line); quit {
					return 0
				}
				if con.commandConn() == nil {
					con.prompt()
				}
			case 0x7f, 0x08: // backspace
				con.mu.Lock()
				if len(con.line) > 0 {
					con.line = con.line[:len(con.line)-1]
				}
				con.mu.Unlock()
				con.redraw()
			case 0x03: // Ctrl-C clears the line rather than killing the console
				con.mu.Lock()
				con.line = nil
				con.mu.Unlock()
				con.out("\r\n")
				con.prompt()
			case 0x04: // Ctrl-D
				con.mu.Lock()
				empty := len(con.line) == 0
				con.mu.Unlock()
				if empty {
					con.out("\r\n")
					return 0
				}
			default:
				if b >= 0x20 {
					con.mu.Lock()
					con.line = append(con.line, rune(b))
					con.mu.Unlock()
					con.redraw()
				}
			}
		}
	}
}

// submit runs one line. Builtins are only the things a shell cannot do for
// us: everything else is handed to the pod, where the usual rules apply.
func (con *console) submit(line string) (quit bool) {
	switch {
	case line == "":
		return false
	case line == "exit", line == "quit":
		con.out("[pod " + con.pod + " is still running — vpctl down " + con.pod +
			" to stop it]\r\n")
		return true
	case line == "tree", strings.HasPrefix(line, "tree "):
		con.runLocalView(line)
		return false
	case line == "hosts", line == "machines":
		con.listPlaces()
		return false
	case line == "vpctl cd", strings.HasPrefix(line, "vpctl cd "):
		// Switching machines is its own command precisely so that plain `cd`
		// never has to guess whether "@gpu03" is a machine or a directory
		// somebody named that.
		con.switchMachine(strings.TrimSpace(strings.TrimPrefix(line, "vpctl cd")))
		return false
	case line == "cd", strings.HasPrefix(line, "cd "):
		con.changeDir(strings.TrimSpace(strings.TrimPrefix(line, "cd")))
		return false
	case strings.HasPrefix(line, "use "):
		con.use(strings.TrimSpace(strings.TrimPrefix(line, "use ")))
		return false
	}
	con.runInPod([]string{"/bin/sh", "-c", line})
	return false
}

// runInPod hands the whole terminal over. Anything that wants a real tty —
// an editor, an agent — gets one, and gets it back on exit.
//
// It waits here rather than returning to the prompt, because a cockpit with
// two things competing for the keyboard is not a cockpit. The read loop keeps
// running throughout; it just forwards instead of editing.
func (con *console) runInPod(argv []string) {
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	state, err := attachSend(c, &proto.Msg{
		Op: proto.OpSession, Pod: con.pod, Session: con.session,
		Argv: argv, Env: os.Environ(), Cwd: con.cwd,
	})
	if err != nil {
		c.Close()
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	con.mu.Lock()
	con.cmdConn = c
	con.mu.Unlock()

	// Do not wait here: the read loop that called this is the same loop that
	// has to forward keystrokes to the command. Hand the terminal over and
	// let the loop carry on.
	keep := c
	go func() {
		defer keep.Close()
		code, detached, err := attachWait(keep)
		con.mu.Lock()
		con.cmdConn = nil
		con.mu.Unlock()
		state.Restore()
		// The command's own exit event usually lands a moment after it
		// stops writing; give it that moment so its route appears with it.
		time.Sleep(120 * time.Millisecond)
		con.flushHeld()
		switch {
		case err != nil:
			con.out("vibepod: " + err.Error() + "\r\n")
		case detached:
			con.out("\r\n[left running — vpctl attach " + con.pod + "]\r\n")
		case code != 0:
			con.out(fmt.Sprintf("[exit %d]\r\n", code))
		}
		con.prompt()
	}()
}

func (con *console) commandConn() *proto.Conn {
	con.mu.Lock()
	defer con.mu.Unlock()
	return con.cmdConn
}

// changeDir is plain cd, and only that: a filesystem operation on paths.
//
// It deliberately knows nothing about machines. A directory can legitimately be
// named "@gpu03", and a cd that guessed between the two would be wrong in a way
// that silently moved you to another machine.
func (con *console) changeDir(arg string) {
	home := os.Getenv("HOME")
	dir := arg
	switch {
	case dir == "":
		// In a cockpit the useful answer to a bare cd is where you could go.
		con.listPlaces()
		return
	case dir == "~":
		dir = home
	case dir == "-":
		if con.prev == "" {
			con.out("cd: no previous directory\r\n")
			return
		}
		dir = con.prev
	case strings.HasPrefix(dir, "~/"):
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
	case !strings.HasPrefix(dir, "/"):
		dir = filepath.Join(con.cwd, dir)
	}
	dir = filepath.Clean(dir)
	before := con.cwd
	con.moveTo(dir)
	if arg == "-" && con.cwd != before {
		// Shells print where they landed, because "-" does not say.
		con.out(short(dir) + "\r\n")
	}
}

// switchMachine moves to the directory a machine owns. The console is the one
// caller that can do this, because the directory it changes is its own.
func (con *console) switchMachine(arg string) {
	if arg == "" {
		con.listPlaces()
		return
	}
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	defer c.Close()
	dir, _, err := machineDir(c, con.pod, arg)
	if err != nil {
		con.out(strings.ReplaceAll(err.Error(), "\n", "\r\n") + "\r\n")
		return
	}
	con.moveTo(dir)
}

// listPlaces answers "where can I go", which in a pod is a question about
// machines rather than paths.
func (con *console) listPlaces() {
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpHosts, Pod: con.pod})
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	con.out(strings.ReplaceAll(renderHosts(reply.Hosts), "\n", "\r\n"))
	con.out("  vpctl cd @<machine> to switch\r\n")
}

// moveTo changes directory once the path is known to be the one wanted.
func (con *console) moveTo(dir string) {
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	defer c.Close()
	// Ask the pod, not the host: the pod's view is the one that decides
	// routing, and it is not the same filesystem.
	reply, err := call(c, &proto.Msg{Op: proto.OpStat, Pod: con.pod, Path: dir})
	if err != nil || reply.Code != 0 {
		con.out("cd: " + dir + ": no such directory in the pod\r\n")
		return
	}
	con.prev, con.cwd = con.cwd, dir
}

func (con *console) use(target string) {
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	defer c.Close()
	if _, err := call(c, &proto.Msg{Op: proto.OpUse, Pod: con.pod,
		Session: con.session, Target: target}); err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	con.pin = target
	con.out("[commands from this console now run on " + target + "]\r\n")
}

func (con *console) runLocalView(line string) {
	args := strings.Fields(line)[1:]
	if len(args) == 0 {
		args = []string{con.pod}
	}
	con.state.Restore()
	if err := cmdTree(args); err != nil {
		fmt.Println("vpctl:", err)
	}
	st, _ := term.MakeRaw(os.Stdin.Fd())
	con.state = st
}

// followEvents prints the live exec log above the prompt. It is the reason
// the console exists: you can see where every command went, as it goes.
func (con *console) followEvents() {
	c, err := connect()
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Send(&proto.Msg{Op: proto.OpLog, Pod: con.pod, Follow: true}); err != nil {
		return
	}
	for {
		m, _, err := c.Recv()
		if err != nil {
			return
		}
		if m.Op != proto.OpEvent {
			continue
		}
		var e event.Event
		if json.Unmarshal(m.Event, &e) != nil || e.Kind != event.KindExit {
			continue
		}
		con.above(logLine(e, false, false))
	}
}

func (con *console) header() {
	con.out("\x1b[2J\x1b[H")
	con.out(fmt.Sprintf("vibepod · %s\r\n", con.pod))
	con.listPlaces()
	con.out("  Ctrl-\\ detaches · `hosts` · `tree` · `use <machine>` · `exit`\r\n\r\n")
}

// above prints a line without disturbing what is being typed.
//
// While a command owns the terminal it prints nothing at all. Redrawing the
// prompt starts by erasing the current line, and the current line belongs to
// whatever is running — an ls, a compiler, an agent. Those lines are held and
// flushed when the terminal comes back, which also puts each command's route
// directly under its own output.
func (con *console) above(line string) {
	if line == "" {
		return
	}
	con.mu.Lock()
	defer con.mu.Unlock()
	if con.cmdConn != nil {
		con.held = append(con.held, line)
		return
	}
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s\r\n%s", line, con.promptText())
}

// flushHeld prints what arrived while a command had the screen.
func (con *console) flushHeld() {
	con.mu.Lock()
	held := con.held
	con.held = nil
	con.mu.Unlock()
	for _, line := range held {
		fmt.Fprintf(os.Stdout, "\r\x1b[K%s\r\n", line)
	}
}

func (con *console) prompt() { con.redraw() }

func (con *console) redraw() {
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s", con.promptText())
}

// promptText shows the full path and the machine that path runs on. The path
// is long, and that is the honest cost of mounting a remote directory at its
// own absolute path; the machine is the thing you most need to know before
// pressing return.
func (con *console) promptText() string {
	dec := route.Resolve(con.table, con.cwd, con.pin)
	label := "@" + dec.Target
	if con.pin != "" && con.pin != "auto" {
		// A pin overrides the directory, so it must be visible: it is the one
		// route the directory cannot explain.
		label += " pinned"
	}
	return fmt.Sprintf("%s (%s) ❯ %s", short(con.cwd), label, string(con.line))
}

// promptPath keeps the prompt short enough to type in front of. Remote
// directories mount at their own absolute path, which is the right choice
// everywhere except here, where it can be sixty characters of prefix.
func promptPath(p string) string {
	p = short(p)
	const max = 28
	if len(p) <= max {
		return p
	}
	parts := strings.Split(p, "/")
	for i := len(parts) - 1; i > 0; i-- {
		tail := strings.Join(parts[i:], "/")
		if len(tail) > max-2 {
			return "…/" + parts[len(parts)-1]
		}
		if i == 1 || len(tail)+len(parts[i-1])+3 > max {
			return "…/" + tail
		}
	}
	return p
}

func (con *console) out(s string) {
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprint(os.Stdout, s)
}
