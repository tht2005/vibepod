package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"vibepod/internal/proto"
	"vibepod/internal/sys"
	"vibepod/internal/term"
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
		return nil, fmt.Errorf("this needs a terminal; use `vpctl run -- cmd` instead")
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
// to be that session: vpctl shell, vpctl attach, vpctl run on a terminal.
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
		fmt.Printf("\r\n[detached — vpctl attach %s to return]\r\n", m.Pod)
	}
	return code, nil
}

// cmdShell opens another independent terminal on a running pod, the way
// `docker exec -it` does. A pod supports as many as you like, each with its
// own working directory.
func cmdShell(args []string) int {
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
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	code, err := attachOwningStdin(c, &proto.Msg{
		Op: proto.OpSession, Pod: m.Spec.Name, Argv: []string{shell},
		Env: os.Environ(), Cwd: podCwd(m.Spec.Binds),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	return code
}

// cmdAttach reconnects to a session that kept running without you.
func cmdAttach(args []string) int {
	pod, session := "", ""
	if len(args) > 0 {
		pod = args[0]
	}
	if len(args) > 1 {
		session = args[1]
	}
	if pod == "" {
		if m, err := loadSpec("", false); err == nil {
			pod = m.Spec.Name
		}
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	defer c.Close()
	code, err := attachOwningStdin(c, &proto.Msg{
		Op: proto.OpAttach, Pod: pod, Session: session,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	return code
}
