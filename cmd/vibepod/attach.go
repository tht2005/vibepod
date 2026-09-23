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

// attachClient hands this terminal to a session in the pod and gets it back
// when the session ends or the user detaches with Ctrl-\.
//
// vpctl stays thin on purpose: the daemon owns the pty and pumps the bytes, so
// the client can be killed, reconnected, or replaced without the session
// noticing.
func attachClient(c *proto.Conn, m *proto.Msg) (int, error) {
	if !term.IsTTY(os.Stdin) {
		return 0, fmt.Errorf("this needs a terminal; use `vpctl run -- cmd` instead")
	}
	state, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return 0, fmt.Errorf("raw mode: %w", err)
	}
	defer state.Restore()

	rows, cols, _ := sys.GetWinsize(os.Stdin.Fd())
	m.TTY, m.Rows, m.Cols = true, rows, cols
	if err := c.Send(m, 0, 1, 2); err != nil {
		return 0, err
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			r, cl, err := sys.GetWinsize(os.Stdin.Fd())
			if err == nil {
				_ = c.Send(&proto.Msg{Op: proto.OpWinch, Rows: r, Cols: cl})
			}
		}
	}()

	reply, _, err := c.Recv()
	if err != nil {
		return 0, err
	}
	switch reply.Op {
	case proto.OpDetach:
		state.Restore()
		fmt.Printf("\r\n[detached from %s — vpctl attach %s to return]\r\n",
			reply.Session, reply.Session)
		return 0, nil
	case proto.OpExit:
		return reply.Code, nil
	case proto.OpErr:
		return 0, fmt.Errorf("%s", reply.Err)
	}
	return 0, fmt.Errorf("unexpected reply %q", reply.Op)
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
	code, err := attachClient(c, &proto.Msg{
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
	code, err := attachClient(c, &proto.Msg{
		Op: proto.OpAttach, Pod: pod, Session: session,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vpctl:", err)
		return 1
	}
	return code
}
