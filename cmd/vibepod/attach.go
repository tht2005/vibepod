package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"vibepod/internal/pod"
	"vibepod/internal/proto"
	"vibepod/internal/sys"
	"vibepod/internal/term"
	"vibepod/internal/ui"
)

// Attaching a terminal to a session has two halves, deliberately separated:
// the request, and the wait. The console needs to keep its own read loop
// running between the two, because it still owns the keyboard afterwards.

// attachSend puts this terminal into raw mode and asks the daemon for a
// session on it. Output goes to a descriptor the daemon writes directly;
// input comes back as messages, so nothing but this process ever reads this
// terminal.
func attachSend(c *proto.Conn, m *proto.Msg) (*term.State, error) {
	if !term.IsTTY(os.Stdin) {
		return nil, fmt.Errorf("this needs a terminal; use `vp run -- cmd` instead")
	}
	state, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return nil, fmt.Errorf("raw mode: %w", err)
	}
	rows, cols, _ := sys.GetWinsize(os.Stdin.Fd())
	m.TTY, m.Rows, m.Cols = true, rows, cols
	if err := c.Send(m, 0, 1, 2); err != nil {
		state.Restore()
		return nil, err
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if r, cl, err := sys.GetWinsize(os.Stdin.Fd()); err == nil {
				_ = c.Send(&proto.Msg{Op: proto.OpWinch, Rows: r, Cols: cl})
			}
		}
	}()
	return state, nil
}

// attachWait blocks until the session ends or the user detaches.
func attachWait(c *proto.Conn) (code int, detached bool, err error) {
	reply, _, err := c.Recv()
	if err != nil {
		return 0, false, err
	}
	switch reply.Op {
	case proto.OpDetach:
		return 0, true, nil
	case proto.OpExit:
		return reply.Code, false, nil
	case proto.OpErr:
		return 0, false, fmt.Errorf("%s", reply.Err)
	}
	return 0, false, fmt.Errorf("unexpected reply %q", reply.Op)
}

// sendInput forwards keystrokes, treating Ctrl-\ as "leave it running".
// Returns false once the session has been detached from.
func sendInput(c *proto.Conn, b []byte) bool {
	for i, ch := range b {
		if ch == term.DetachKey {
			if i > 0 {
				_ = c.Send(&proto.Msg{Op: proto.OpInput, Data: b[:i]})
			}
			_ = c.Send(&proto.Msg{Op: proto.OpDetach})
			return false
		}
	}
	_ = c.Send(&proto.Msg{Op: proto.OpInput, Data: b})
	return true
}

// attachOwningStdin is the whole flow for a command whose process exists only
// to be that session: vp shell, vp attach, vp run on a terminal.
func attachOwningStdin(c *proto.Conn, m *proto.Msg) (int, error) {
	state, err := attachSend(c, m)
	if err != nil {
		return 0, err
	}
	defer state.Restore()

	ended := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && !sendInput(c, buf[:n]) {
				return
			}
			if err != nil {
				return
			}
			select {
			case <-ended:
				return
			default:
			}
		}
	}()
	code, detached, err := attachWait(c)
	close(ended)
	if err != nil {
		return 0, err
	}
	if detached {
		state.Restore()
		fmt.Printf("\r\n[detached — vp attach %s to return]\r\n", m.Pod)
	}
	return code, nil
}

// cmdShell opens another independent terminal on a running pod, the way
// `docker exec -it` does. A pod supports as many as you like, each with its
// own working directory.
//
// On a terminal it is the block interface (internal/ui); --raw, or anything
// that is not a terminal, gets the shell's own pty exactly as it is.
func cmdShell(args []string) int {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	on := fs.String("on", "", "open it on this machine instead of the pod's default")
	dir := fs.String("C", "", "start in this directory")
	raw := fs.Bool("raw", false, "the shell's own terminal, without the block interface")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := fs.Arg(0)
	// A pod that is already running can be opened from anywhere. Only creating
	// one needs a config, so a missing vibepod.yaml is a reason to skip `up`,
	// not a reason to refuse — you are as likely to want another terminal on a
	// pod from outside its project directory as from inside it.
	m, err := loadSpec(name)
	c, cerr := connect()
	if cerr != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", cerr)
		return 1
	}
	defer c.Close()
	switch {
	case err == nil:
		if err := ensureUp(c, m); err != nil {
			fmt.Fprintln(os.Stderr, "vibepod:", err)
			return 1
		}
	case name == "":
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	default:
		m = &proto.Msg{Spec: &proto.Spec{Name: name}}
	}
	start := podCwd(m.Mounts)
	if *dir != "" {
		start = *dir
	}
	// The pod's own $SHELL, so that an interactive session's commands are
	// recorded like any other. vpsh execs the user's real shell immediately; a
	// session whose backend is another machine never reaches it at all, because
	// that session *is* a shell over there.
	req := &proto.Msg{
		Op: proto.OpSession, Pod: m.Spec.Name, Argv: []string{pod.ShellPath},
		Env: os.Environ(), Cwd: start, Backend: *on,
	}
	var code int
	if *raw || !framable() {
		code, err = attachOwningStdin(c, req)
	} else {
		code, _, err = attachFramed(c, req)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return code
}

// framable is whether the block interface can run here at all.
func framable() bool { return term.IsTTY(os.Stdin) && term.IsTTY(os.Stdout) }

// attachFramed runs a session through the block interface. ok is false, with
// nothing done, when the session is one `vp shell` did not start: its shells
// have no hooks, so it can only be shown raw.
func attachFramed(c *proto.Conn, m *proto.Msg) (code int, ok bool, err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return 0, false, err
	}
	defer r.Close()
	rows, cols, _ := sys.GetWinsize(os.Stdin.Fd())
	m.TTY, m.Framed = true, true
	m.Rows, m.Cols = ui.EmuSize(rows, cols)
	// A terminal the emulator understands, for what runs inside a block. A
	// program that takes the whole screen gets the real terminal, and xterm is
	// what every real terminal speaks.
	if m.Op == proto.OpSession {
		m.Env = setEnv(m.Env, "TERM", "xterm-256color")
	}
	fd := int(w.Fd())
	err = c.Send(m, fd, fd, fd)
	w.Close()
	if err != nil {
		return 0, false, err
	}
	reply, _, err := c.Recv()
	if err != nil {
		return 0, false, err
	}
	switch reply.Op {
	case proto.OpErr:
		return 0, false, fmt.Errorf("%s", reply.Err)
	case proto.OpOK:
		if !reply.Framed {
			return 0, false, nil
		}
	default:
		return 0, false, fmt.Errorf("unexpected reply %q", reply.Op)
	}
	res := ui.RunShell(ui.ShellConfig{Conn: c, Out: r, Pod: m.Pod,
		Session: reply.Session, History: historyPath(), Rows: rows, Cols: cols,
		Dial: dialQuiet})
	if res.Err != nil {
		return 0, true, res.Err
	}
	if res.Detached {
		fmt.Printf("[detached — vp attach %s %s to return]\n", m.Pod, reply.Session)
	}
	return res.Code, true, nil
}

func setEnv(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, k+"=") {
			out = append(out, e)
		}
	}
	return append(out, k+"="+v)
}

// historyPath is `vp shell`'s history: one per user, like a shell's.
func historyPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "vibepod", "history")
}

// cmdAttach reconnects to a session that kept running without you.
func cmdAttach(args []string) int {
	name, session := "", ""
	if len(args) > 0 {
		name = args[0]
	}
	if len(args) > 1 {
		session = args[1]
	}
	if name == "" {
		if m, err := loadSpec(""); err == nil {
			name = m.Spec.Name
		}
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	defer c.Close()
	// A session `vp shell` started comes back as blocks; any other — a console,
	// a raw shell — the way it was.
	if framable() {
		code, ok, err := attachFramed(c, &proto.Msg{
			Op: proto.OpAttach, Pod: name, Session: session,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "vibepod:", err)
			return 1
		}
		if ok {
			return code
		}
	}
	code, err := attachOwningStdin(c, &proto.Msg{
		Op: proto.OpAttach, Pod: name, Session: session,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return code
}
