package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"vibepod/internal/event"
	"vibepod/internal/proto"
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
	target  string

	mu      sync.Mutex
	line    []rune
	state   *term.State
	cmdConn *proto.Conn // non-nil while a command owns the terminal
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
	con := &console{
		spec: m, pod: m.Spec.Name, cwd: podCwd(m.Spec.Binds),
		session: "console", target: "auto",
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
	case strings.HasPrefix(line, "cd "):
		con.changeDir(strings.TrimSpace(strings.TrimPrefix(line, "cd ")))
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

func (con *console) changeDir(dir string) {
	if !strings.HasPrefix(dir, "/") {
		dir = con.cwd + "/" + dir
	}
	c, err := connect()
	if err != nil {
		con.out("vibepod: " + err.Error() + "\r\n")
		return
	}
	defer c.Close()
	// Ask the pod, not the host: the pod's view is the one that decides
	// routing, and it is not the same filesystem.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer devnull.Close()
	fd := int(devnull.Fd())
	reply, err := call(c, &proto.Msg{
		Op: proto.OpSession, Pod: con.pod, Argv: []string{"/usr/bin/test", "-d", dir},
		Env: os.Environ(), Cwd: "/",
	}, fd, fd, fd)
	if err != nil || reply.Code != 0 {
		con.out("cd: " + dir + ": no such directory in the pod\r\n")
		return
	}
	con.cwd = dir
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
	con.target = target
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
		con.above(logLine(e, false))
	}
}

func (con *console) header() {
	con.out("\x1b[2J\x1b[H")
	con.out(fmt.Sprintf("vibepod · %s\r\n", con.pod))
	for _, r := range con.spec.Routes {
		if r.Target != "pod" {
			con.out(fmt.Sprintf("  %s → %s\r\n", short(r.Prefix), r.Target))
		}
	}
	con.out("  Ctrl-\\ detaches a running command · `use <host>` · `tree` · `exit`\r\n\r\n")
}

// above prints a line without disturbing what is being typed.
func (con *console) above(line string) {
	if line == "" {
		return
	}
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s\r\n%s", line, con.promptText())
}

func (con *console) prompt() { con.redraw() }

func (con *console) redraw() {
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s", con.promptText())
}

func (con *console) promptText() string {
	exec := con.target
	if exec == "" {
		exec = "auto"
	}
	return fmt.Sprintf("%s [%s] ❯ %s", short(con.cwd), exec, string(con.line))
}

func (con *console) out(s string) {
	con.mu.Lock()
	defer con.mu.Unlock()
	fmt.Fprint(os.Stdout, s)
}

var _ = time.Second
